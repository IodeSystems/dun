package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// queuedKind is what a buffered item IS, which decides how much it is worth.
//
// The distinction that matters is the last one: a notification or a user
// message is news the model has to act on, so if nothing else picks it up the
// driver runs a turn for it. An aside is context the model needs the next time
// it thinks — not a reason to think. Publishing an aside as an inbox arrival
// would spend a whole turn (and, for a tool-set change, a turn whose only
// content is "ok") on something that costs nothing to carry.
type queuedKind int

const (
	queuedNotification queuedKind = iota // a background job finished
	queuedUser                           // the user typed while the agent worked
	queuedAside                          // context worth knowing, never worth a turn
	queuedPeer                           // another dun session's dun_msg (peers.go)
	queuedPeerQuiet                      // the same, sent with wake:false
	queuedService                        // dund about the service itself: an upgrade coming, or done

	// queuedKindCount is a sentinel — keep it last. It exists so a test can
	// walk every kind and assert it has a queuedPolicies entry, since the
	// fallback for a missing one is silent by design.
	queuedKindCount
)

// queued is something buffered to reach the model at the cheapest moment: a
// background job's completion, a message the user typed while the agent was
// already working, or an aside about the session itself.
type queued struct {
	kind queuedKind
	text string
}

// queuedPolicy is everything the drains need to know about a kind.
//
// A table rather than a switch per drain: this mapping used to live in four
// places (a prefix switch, an Entry-kind mapping, and two partition predicates
// that each spelled out `== queuedAside`), so adding a kind meant finding all
// four. Missing the partition is the dangerous one — the new kind silently
// starts costing a turn, or silently stops being delivered, and nothing errors.
type queuedPolicy struct {
	// prefix labels the item where it is lifted into a tool result, so the
	// model can tell the user talking from the machinery reporting.
	prefix string
	// entryKind is how it is persisted once it reaches the conversation.
	entryKind agent.EntryKind
	// causesTurn is whether news of this kind is worth WAKING the model for.
	// False means it may only ride a turn that is already happening — see the
	// aside rationale on queuedKind. This field IS the flush partition; the
	// drains derive their predicates from it rather than naming kinds.
	causesTurn bool
}

var queuedPolicies = map[queuedKind]queuedPolicy{
	queuedNotification: {prefix: "[background] ", entryKind: agent.KindNotification, causesTurn: true},
	queuedUser:         {prefix: "[user] ", entryKind: agent.KindUser, causesTurn: true},
	queuedAside:        {prefix: "[session] ", entryKind: agent.KindNotification, causesTurn: false},
	// A peer message carries its own label too (PeerLabel), so it reads the
	// same flushed as lifted: never as the user.
	queuedPeer:      {prefix: "[peer] ", entryKind: agent.KindNotification, causesTurn: true},
	queuedPeerQuiet: {prefix: "[peer] ", entryKind: agent.KindNotification, causesTurn: false},
	// dund's own word to the session (an upgrade), labelled so it is never
	// read as the person's.
	queuedService: {prefix: "[dun] ", entryKind: agent.KindNotification, causesTurn: true},
}

// policy returns the kind's entry in the table. An unregistered kind falls back
// to the notification policy — the failure leans toward DELIVERING news at the
// cost of a turn, because losing it outright is the worse of the two.
func (q queued) policy() queuedPolicy {
	if p, ok := queuedPolicies[q.kind]; ok {
		return p
	}
	return queuedPolicies[queuedNotification]
}

// Notify hands the model a proactive notification (a finished background job).
//
// The note is buffered rather than published straight into the inbox, because
// WHEN it reaches the model decides how much it costs:
//
//   - If a tool call is in flight, the note rides back inside that tool's
//     result (see liftNotes). The model is already waiting on that result, so
//     the news costs no extra turn and cannot land as a stray assistant message.
//   - Otherwise flushNotes publishes whatever is left when the turn ends, as
//     one inbox arrival per note, and wakes the driver ONCE for all of them.
//
// OnNotify still fires immediately so a UI shows the job finishing in real time.
func (h *Harness) Notify(text string) {
	h.workNote(text)
	h.noteMu.Lock()
	h.queue = append(h.queue, queued{kind: queuedNotification, text: text})
	h.noteMu.Unlock()
	if cb := h.store.notifyCallback(); cb != nil {
		cb(text)
	}
}

// Say hands the model a user message that arrived while it was already working.
//
// Same machinery as Notify, and for the same reason: the cheapest place for news
// is inside a tool result the model is already going to read. A message typed
// mid-turn therefore reaches it WITHIN the running turn — no extra round-trip, no
// waiting for the agent to finish, and no assistant message stacked on another.
//
// The buffer is a slice, so this composes without limit: tool call + message +
// message + tool call delivers both messages on the next result, and anything
// typed after that rides the one after. Nothing is dropped — whatever the turn
// does not pick up is published by flushQueued when it ends.
//
// A tool call blocked in a wait (exec's grace, agent wait:true) would hold the
// message for as long as it waits — an hour, for exec timeout:3600. So Say
// also releases those waits: see userWaiting.
func (h *Harness) Say(text string) {
	h.workInput(text)
	h.noteMu.Lock()
	h.queue = append(h.queue, queued{kind: queuedUser, text: text})
	if h.userWaitCh != nil {
		close(h.userWaitCh)
		h.userWaitCh = nil
	}
	h.noteMu.Unlock()
}

// closedCh is a channel that is always ready.
var closedCh = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// userWaiting is ready once the user has a message waiting for the model. A
// tool that blocks adds it to its select and, when it fires, returns what it
// has, so the message rides that result (liftQueued) instead of waiting out
// the tool.
//
// Ready at once if a message is ALREADY queued: one typed while the model was
// still writing the call must not wait for it either. Checked and fetched under
// the same lock Say closes it under, so no message falls between the two.
func (h *Harness) userWaiting() <-chan struct{} {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	for _, q := range h.queue {
		if q.kind == queuedUser || q.kind == queuedPeer {
			return closedCh
		}
	}
	if h.userWaitCh == nil {
		h.userWaitCh = make(chan struct{})
	}
	return h.userWaitCh
}

// Aside hands the model context about the session itself — today, that the tool
// set changed under it (/rag on, /lsp off).
//
// It rides the same buffer as Notify and Say, and differs in exactly one way:
// it will never CAUSE a turn. The three ways it can reach the model, cheapest
// first:
//
//   - lifted into a tool result the model is already reading (liftQueued),
//   - published just before the next turn that was going to run anyway
//     (flushSilent, from prepareTurn) — including the turn a user message
//     starts, which is the "join with the user message" case,
//   - otherwise it waits. Indefinitely, and that is correct: a tool set the
//     model never gets to use costs nothing to leave unsaid.
//
// Why not just let the tool schemas speak for themselves? They do travel in
// every request, so the model CAN see the new set — but not that it changed
// mid-conversation, and a model that reasoned two turns ago about not having
// search will keep acting on that. The aside is the delta, not the schemas.
func (h *Harness) Aside(text string) {
	h.noteMu.Lock()
	h.queue = append(h.queue, queued{kind: queuedAside, text: text})
	h.noteMu.Unlock()
}

// ForceToolCall queues a tool call to be injected into the next LLM response's
// tool_calls before they are persisted or dispatched. The call appears as if
// the model made it: it is persisted as assistant(tool_calls) → tool(result),
// and the Turn loop dispatches it normally.
//
// This is the mechanism for host-initiated actions that must look like the
// model's own decisions — a forced call makes
// the exchange a proper tool call/result pair, not a disembodied
// Aside notification.
func (h *Harness) ForceToolCall(tc llm.ToolCall) {
	h.noteMu.Lock()
	h.forcedToolCalls = append(h.forcedToolCalls, tc)
	h.noteMu.Unlock()
}

// mergeForcedToolCalls is the OnToolCalls callback installed on the Session.
// It drains any host-queued tool calls (from ForceToolCall) and prepends them
// to the model's own tool calls, so the Turn loop persists and dispatches all
// of them as if the model had generated them.
func (h *Harness) mergeForcedToolCalls(tcs []llm.ToolCall) []llm.ToolCall {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	if len(h.forcedToolCalls) == 0 {
		return tcs
	}
	h.forcedCallsTotal += len(h.forcedToolCalls)
	merged := make([]llm.ToolCall, 0, len(h.forcedToolCalls)+len(tcs))
	merged = append(merged, h.forcedToolCalls...)
	merged = append(merged, tcs...)
	h.forcedToolCalls = nil
	return merged
}

// liftQueued drains the buffer into a tool result, so news that arrived mid-turn
// is reported inside the result the model is already reading. Returns result
// unchanged when nothing is buffered.
func (h *Harness) liftQueued(result string) string {
	// Not into a turn the user interrupted: the buffer is what starts the next
	// turn (see interruptState.interrupting).
	if h.interrupting.Load() {
		return result
	}
	// Everything, regardless of policy: a result is already on its way to the
	// model, so even an aside rides it for free.
	items := h.drain(func(queued) bool { return true })
	if len(items) == 0 {
		return result
	}
	// Track notifications that were smuggled into this tool result.
	h.noteMu.Lock()
	for _, q := range items {
		if q.kind == queuedNotification {
			h.notificationsLifted++
		}
	}
	h.noteMu.Unlock()
	var b strings.Builder
	// Trailing newlines on the tool's own output would compound with the separator
	// below into a run of blank lines.
	b.WriteString(strings.TrimRight(result, "\n"))
	for _, q := range items {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(q.policy().prefix)
		b.WriteString(q.text)
	}
	return b.String()
}

// drain removes every buffered item matching pred and returns it, leaving the
// rest for a later delivery. One partition under one lock, so an item is never
// taken by two drains nor missed by both.
func (h *Harness) drain(pred func(queued) bool) []queued {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	var taken, keep []queued
	for _, q := range h.queue {
		if pred(q) {
			taken = append(taken, q)
		} else {
			keep = append(keep, q)
		}
	}
	h.queue = keep
	return taken
}

// flushQueued publishes whatever the turn did not pick up, as real inbox
// arrivals — user messages as user turns, notifications as notifications.
// Returns how many were flushed so the caller can decide whether a follow-up turn
// is worth running.
//
// Takes exactly the kinds whose policy says causesTurn, because publishing as an
// inbox arrival is what makes a driver run a turn. The rest stay buffered until
// a turn happens for some other reason (flushSilent) or a tool result carries
// them.
func (h *Harness) flushQueued() int {
	items := h.drain(func(q queued) bool { return q.policy().causesTurn })
	for _, q := range items {
		h.store.publish(agent.Entry{
			ID: uuid.New().String(), Kind: q.policy().entryKind,
			Content: q.text, CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(items)
}

// flushSilent appends the buffered items that must never CAUSE a turn to the
// conversation, without marking them inbox arrivals, immediately before a turn
// about to run for some other reason. The model reads them in that turn's
// context; nothing schedules a turn on their account (that is the whole point —
// see Aside, today the only such kind).
//
// The exact complement of flushQueued's partition, by construction: both read
// causesTurn off the same table.
func (h *Harness) flushSilent() int {
	items := h.drain(func(q queued) bool { return !q.policy().causesTurn })
	for _, q := range items {
		h.store.appendSilent(agent.Entry{
			ID: uuid.New().String(), Kind: q.policy().entryKind,
			Content: q.text, CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(items)
}

// Pending reports whether a Continue turn has anything new to process —
// unclaimed inbox arrivals, buffered messages, or forced tool calls.
// Asides do not count: they are carried by a turn, never a reason to run one.
func (h *Harness) Pending() int {
	h.noteMu.Lock()
	buffered := 0
	for _, q := range h.queue {
		if q.policy().causesTurn {
			buffered++
		}
	}
	forced := len(h.forcedToolCalls)
	h.noteMu.Unlock()
	return h.store.pending() + buffered + forced
}

// Unqueue takes back the person's messages the running turn has not taken
// yet and returns them, oldest first (USER 2026-10-27: up with queued
// messages loads them back into the composer, merged, to fix: "enter oops
// fix"). One the turn already took stays where it is.
func (h *Harness) Unqueue() []string {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	var out []string
	kept := h.queue[:0]
	for _, q := range h.queue {
		if q.kind == queuedUser {
			out = append(out, q.text)
			continue
		}
		kept = append(kept, q)
	}
	h.queue = kept
	return out
}

// Queued reports how many buffered messages are waiting to reach the model, so a
// UI can say "1 queued" rather than leaving the user wondering whether what they
// typed went anywhere.
func (h *Harness) Queued() int {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	n := 0
	for _, q := range h.queue {
		if q.policy().causesTurn { // an aside: the user did not type it; do not count it
			n++
		}
	}
	return n
}

// Wake fires when a background job finishes, so the driver runs a Continue turn.
func (h *Harness) Wake() <-chan struct{} { return h.wake }

// BackgroundRunning is how many background jobs are still in flight.
func (h *Harness) BackgroundRunning() int {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	return h.bgRun
}

// noteServerNotification lifts an unsolicited MCP notification into the
// conversation.
//
// Notify, not a log line: the whole point is that the MODEL learns of it. A
// merge conflict appearing under a running agent is the motivating case —
// from that moment every symbol in the file may combine both sides and an
// edit can corrupt the merge, and the agent would otherwise find out only if
// it happened to ask again.
//
// Runs on the MCP client's read goroutine, so it hands off and returns:
// Notify appends to a queue under a mutex and never blocks on a turn.
//
// Only notifications/message is lifted. The other MCP notification methods
// are protocol bookkeeping (tool/resource list changes, progress) that the
// manager already acts on or that mean nothing to a conversation, and
// forwarding them would train the model to skim what it sees here.
func (h *Harness) noteServerNotification(n mcpmgr.Notification) {
	if n.Method != "notifications/message" {
		return
	}
	// An index report is the server's state, not news for the model: dun
	// acts on it and says what it did (indexstate.go).
	if in, ok := parseIndexNote(n.Params); ok {
		h.noteIndex(n.ServerID, in)
		return
	}
	// So is an install-hint report: kept, and said once, quietly.
	if rep, ok := parseSetupNote(n.Params); ok {
		if line := h.noteSetup(n.ServerID, rep); line != "" && h.cfg.OnServerSetup != nil {
			h.cfg.OnServerSetup(n.ServerID, line)
		}
		return
	}
	// A server's daemon upgrading, upgraded or failing (poly-lsp-mcp/daemon)
	// is for the person, one quiet line, not the model: it changes nothing
	// the model can act on.
	if msg, ok := parseDaemonNote(n.Params); ok {
		if h.cfg.OnServerSetup != nil {
			h.cfg.OnServerSetup(n.ServerID, msg)
		}
		return
	}
	text := strings.TrimSpace(n.Text())
	if text == "" {
		return
	}
	// A notice that is JSON is a server's state for a program to read
	// (mcpshell's hosted sizes, which dun reads from its stderr), not news:
	// it goes to the server's activity log, which /mcp shows, not into the
	// conversation (USER 2026-10-27: spurious, keep it available, off by
	// default).
	if (strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")) && json.Valid([]byte(text)) {
		h.noteServer(n.ServerID, "notice: %s", oneLine(text, 400))
		return
	}
	// Name the server. Several are usually running, and "a merge conflict
	// appeared" is a different fact depending on which workspace said it.
	h.Notify(fmt.Sprintf("[%s] %s", n.ServerID, text))
}

// withLiftedQueue appends anything buffered during a tool call to that tool's
// result — a background job that finished, a message the user typed. The model is
// already reading the result, so the news costs no extra turn and, critically, no
// assistant message is appended after another assistant message.
func withLiftedQueue(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		out, err := inner(ctx, tc)
		if err != nil {
			return out, err
		}
		return h.liftQueued(out), nil
	}
}
