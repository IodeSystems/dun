// Package dun is a coding-agent harness: it composes agentkit (the engine) with
// three MCP tool servers — poly-lsp-mcp (semantic code), mcpshell (sandboxed
// compute), and raglit (docs/RAG) — into one agent that works a task inside an
// isolated workspace.
//
// Slice 1 is the headless composition: spawn the servers, bridge their tools
// into an agent.Session, run the tool loop. The Bubble Tea TUI and the Docker +
// git-worktree isolation layer on top (see plan/plan.md).
package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Server is one MCP tool server dun spawns.
type Server struct {
	ID      string
	Command string
	Args    []string
	// Env entries ("KEY=value") for the spawned server — a per-machine DSN or
	// endpoint that cannot be baked into a committed config.
	Env []string
	// Timeout in seconds for tool discovery; 0 uses dun's default.
	Timeout int
	// Autostart spawns this server when the session starts. Off means the
	// server is available but idle until asked for (/lsp on, /rag on).
	Autostart bool
}

// DefaultServers points the three tool servers at a workspace directory (later a
// git worktree, and later still `docker exec` into a container).
//
// Only shell autostarts. The other two are opt-in because each costs real
// startup time and neither is universally wanted: poly-lsp-mcp indexes the
// repo, and raglit needs the workspace ingested before it answers anything.
// A session that is going to run a couple of shell commands should not pay for
// either, and — more to the point — a machine missing one of those binaries
// should still get a working dun. Persist a preference with /rag auto or
// /lsp auto (see SetAutostart).
func DefaultServers(workspace, raglitHome string) []Server {
	return []Server{
		{ID: "code", Command: "poly-lsp-mcp", Args: []string{"mcp", "--root", workspace}},
		{ID: "shell", Command: "mcpshell", Args: []string{"mcp", "--files-dir", workspace}, Autostart: true},
		// --embedded: dun's raglit home is a per-session temp dir, so the index
		// is single-session and in-process. Without it raglit routes to the
		// shared daemon, which refuses a client that has no project name and
		// exits before the MCP handshake ("transport closed").
		{ID: "docs", Command: "raglit", Args: []string{"serve", "--embedded", "--home", raglitHome}},
	}
}

// Server ids dun knows by name. The commands are /lsp and /rag because that is
// what the things are called; the ids stay tool-family names.
const (
	ServerCode  = "code"  // poly-lsp-mcp — /lsp
	ServerShell = "shell" // mcpshell
	ServerDocs  = "docs"  // raglit — /rag
)

// Config configures a dun harness.
type Config struct {
	Workspace  string
	RaglitHome string

	Servers []Server // nil → DefaultServers(Workspace, RaglitHome)
	// ConfigDir is where dun.json / dun.local.json are looked for. Empty →
	// Workspace. Separated because the workspace may be an isolated worktree
	// while the config lives with the developer's checkout.
	ConfigDir string
	// AutostartOverride forces a server's autostart for THIS run only
	// (dun --rag / --lsp), above both the built-in default and the config
	// files. Nothing is persisted — that is what /rag auto is for.
	AutostartOverride map[string]bool
	Client      agent.LLMRunner // the LLM (e.g. *llm.Client)
	System      string          // nil → defaultSystem
	Exec        ExecBackend     // nil → no exec tool; else adds the built-in exec tool
	Ask         AskFunc         // nil → no ask_user tool; else adds the human-in-the-loop tool
	Worktree    *Worktree       // the session worktree (for open_pr)
	EnablePR    bool            // add the open_pr tool (opt-in: pushing + PR is outward-facing)
	SessionFile string          // persist the conversation here (resumable); "" = in-memory only
	OnToken     func(string)
	OnToolCall  func(tool string, args map[string]any, result string)
	// OnNotify fires when a plain notification (KindNotification) is injected
	// into the conversation (e.g. a background job's completion).
	OnNotify func(text string)
	// OnDocs fires when the proactive-RAG preparer surfaces relevant documents —
	// one aggregated summary per pass (found/surfaced counts + surfaced docs).
	OnDocs func(DocsNote)
	// OnRetry fires while dun is waiting on the provider — every backoff, the
	// recovery, and the give-up, at both request and turn scope. Without it the
	// wait is invisible: the retries are logged, and a TUI's logs are not on
	// screen. See RetryNote.
	OnRetry func(RetryNote)
}

// Harness is a running dun: the MCP manager + an agent Session over its tools.
type Harness struct {
	mgr     *mcpmgr.Manager
	Session *agent.Session
	Tools   []mcpmgr.MCPTool
	store   *sessionStore
	client  agent.LLMRunner // kept for its retry policy (see turnRetryPolicy)
	onRetry func(RetryNote)
	wake    chan struct{} // signals a driver to run a Continue turn (bg job done)
	bgMu    sync.Mutex
	bgSeq   int
	bgRun   int // background jobs still running
	noteMu  sync.Mutex
	queue   []queued // messages not yet delivered to the model

	// Servers can start and stop mid-session (/rag on, /lsp off), which means
	// the tool set is not fixed at construction: srvMu guards the spec list and
	// the last-error map, and every change ends in applyTools rebuilding the
	// Session's tools, dispatcher, system prompt and doc preparer.
	cfg     Config
	srvMu   sync.Mutex
	specs   []Server          // every configured server, running or not
	lastErr map[string]string // id → why its last start attempt failed
	// turnMu is held for the length of a turn. A server command arrives on
	// another goroutine (the -p reader), and swapping the Session's tools out
	// from under a running turn is a data race — so a rebuild that cannot take
	// the lock sets applyPending and the turn applies it on the way out.
	turnMu       sync.Mutex
	applyPending atomic.Bool
	// toolsInit is false until the first rebuild, so the initial tool set is
	// not announced as a CHANGE — the system prompt already describes it.
	toolsInit bool
}

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
)

// queued is something buffered to reach the model at the cheapest moment: a
// background job's completion, a message the user typed while the agent was
// already working, or an aside about the session itself.
type queued struct {
	kind queuedKind
	text string
}

// prefix labels a queued item where it is lifted into a tool result, so the model
// can tell the user talking from the machinery reporting.
func (q queued) prefix() string {
	switch q.kind {
	case queuedUser:
		return "[user] "
	case queuedAside:
		return "[session] "
	default:
		return "[background] "
	}
}

// Resumed reports how many entries were restored from an existing session file.
func (h *Harness) Resumed() int { return h.store.Loaded() }

// HistoryItem is one replayable conversation entry for a resuming client — the
// neutral shape the -p `history` event carries so a TUI can rebuild scrollback
// without re-running any turn. A tool_call and its result share a CallID so the
// client folds them into one block.
type HistoryItem struct {
	Kind    string         `json:"kind"` // user|assistant|tool_call|tool_result|notification
	Content string         `json:"content,omitempty"`
	Tool    string         `json:"tool,omitempty"`    // tool_call / tool_result
	CallID  string         `json:"call_id,omitempty"` // pairs a tool_call with its result
	Args    map[string]any `json:"args,omitempty"`    // decoded tool_call arguments
}

// History returns the loaded conversation as replayable items in chronological
// order, for a resuming client to render as scrollback. Empty for a fresh
// session. Compaction markers are dropped (a resumed client shows the surviving
// entries, not the fold marker) and empty assistant turns are skipped;
// tool-call arguments are decoded from the stored JSON so the client renders
// them like a live call.
func (h *Harness) History() []HistoryItem {
	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		return nil
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		return entries[i].ID < entries[j].ID
	})
	out := make([]HistoryItem, 0, len(entries))
	for _, e := range entries {
		switch e.Kind {
		case agent.KindUser:
			out = append(out, HistoryItem{Kind: "user", Content: e.Content})
		case agent.KindAssistant:
			if strings.TrimSpace(e.Content) == "" {
				continue
			}
			out = append(out, HistoryItem{Kind: "assistant", Content: e.Content})
		case agent.KindToolCall:
			var args map[string]any
			_ = json.Unmarshal([]byte(e.Content), &args)
			out = append(out, HistoryItem{Kind: "tool_call", Tool: e.ToolName, CallID: e.ToolCallID, Args: args})
		case agent.KindToolResult:
			out = append(out, HistoryItem{Kind: "tool_result", Tool: e.ToolName, CallID: e.ToolCallID, Content: e.Content})
		case agent.KindNotification:
			out = append(out, HistoryItem{Kind: "notification", Content: e.Content})
		}
	}
	return out
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
func (h *Harness) Say(text string) {
	h.noteMu.Lock()
	h.queue = append(h.queue, queued{kind: queuedUser, text: text})
	h.noteMu.Unlock()
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
//     (flushAsides, from prepareTurn) — including the turn a user message
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

// liftQueued drains the buffer into a tool result, so news that arrived mid-turn
// is reported inside the result the model is already reading. Returns result
// unchanged when nothing is buffered.
func (h *Harness) liftQueued(result string) string {
	h.noteMu.Lock()
	items := h.queue
	h.queue = nil
	h.noteMu.Unlock()
	if len(items) == 0 {
		return result
	}
	var b strings.Builder
	// Trailing newlines on the tool's own output would compound with the separator
	// below into a run of blank lines.
	b.WriteString(strings.TrimRight(result, "\n"))
	for _, q := range items {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(q.prefix())
		b.WriteString(q.text)
	}
	return b.String()
}

// flushQueued publishes whatever the turn did not pick up, as real inbox
// arrivals — user messages as user turns, notifications as notifications.
// Returns how many were flushed so the caller can decide whether a follow-up turn
// is worth running.
//
// Asides are NOT flushed here: publishing one as an inbox arrival is what would
// make a driver run a turn for it. They stay buffered until a turn happens for
// some other reason (flushAsides) or a tool result carries them.
func (h *Harness) flushQueued() int {
	h.noteMu.Lock()
	var items, keep []queued
	for _, q := range h.queue {
		if q.kind == queuedAside {
			keep = append(keep, q)
			continue
		}
		items = append(items, q)
	}
	h.queue = keep
	h.noteMu.Unlock()
	for _, q := range items {
		kind := agent.KindNotification
		if q.kind == queuedUser {
			kind = agent.KindUser
		}
		h.store.publish(agent.Entry{
			ID: uuid.New().String(), Kind: kind,
			Content: q.text, CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(items)
}

// flushAsides appends buffered asides to the conversation WITHOUT marking them
// inbox arrivals, immediately before a turn that is about to run for some other
// reason. The model reads them in that turn's context; nothing schedules a turn
// on their account (that is the whole point — see Aside).
func (h *Harness) flushAsides() int {
	h.noteMu.Lock()
	var items, keep []queued
	for _, q := range h.queue {
		if q.kind == queuedAside {
			items = append(items, q)
			continue
		}
		keep = append(keep, q)
	}
	h.queue = keep
	h.noteMu.Unlock()
	for _, q := range items {
		h.store.appendSilent(agent.Entry{
			ID: uuid.New().String(), Kind: agent.KindNotification,
			Content: q.text, CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(items)
}

// Pending reports whether a Continue turn has anything new to process —
// unclaimed inbox arrivals or buffered messages. Asides do not count: they are
// carried by a turn, never a reason to run one.
func (h *Harness) Pending() int {
	h.noteMu.Lock()
	buffered := 0
	for _, q := range h.queue {
		if q.kind != queuedAside {
			buffered++
		}
	}
	h.noteMu.Unlock()
	return h.store.pending() + buffered
}

// Queued reports how many buffered messages are waiting to reach the model, so a
// UI can say "1 queued" rather than leaving the user wondering whether what they
// typed went anywhere.
func (h *Harness) Queued() int {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	n := 0
	for _, q := range h.queue {
		if q.kind != queuedAside { // the user did not type it; do not count it
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

// Continue runs a turn with NO new user message — to process pending
// notifications (e.g. a background job's completion). This is the converge
// point: the notification (+ any queued messages) coalesce into one turn.
func (h *Harness) Continue(ctx context.Context) (agent.TurnResult, error) {
	// Publish anything a tool result did not already carry (and pair off any
	// interrupted tool call), so the turn below has it — and so several jobs
	// finishing at once become ONE turn.
	h.prepareTurn(ctx)
	if h.store.pending() == 0 {
		// Nothing new to react to. Running the model anyway appends an assistant
		// message directly after the previous one, which providers reject on the
		// following request. A duplicate wake is a no-op, not a turn.
		return agent.TurnResult{}, nil
	}
	return h.runTurn(ctx, h.Session.Turn)
}

// prepareTurn readies the store for a turn (or a retry of one).
//
// Two jobs, in this order because the first constrains the second:
//
//  1. Pair off any tool call that never got a result. A turn killed between
//     persisting a call and recording its result leaves the history structurally
//     invalid — providers deserialize historical tool_calls, and an
//     assistant(tool_calls) with no matching tool message is rejected before the
//     model is reached — so every later request fails identically. Healing it is
//     what makes "just send another message" actually resume the session.
//  2. Deliver everything buffered. When step 1 wrote a result, the buffer rides
//     INSIDE it, which is the whole point: the user's message and the recovery
//     from the dropped connection travel together, in one batch, costing no extra
//     turn. Otherwise the buffer is published as ordinary inbox arrivals.
//
// Called before every attempt in runTurn, so a message typed during a backoff
// wait joins the retry rather than queuing behind it.
func (h *Harness) prepareTurn(ctx context.Context) {
	if h.healOrphanToolCalls(ctx) == 0 {
		h.flushQueued()
	}
	// Asides ride whatever turn is about to run — including the one a user
	// message starts, which is where "the tool set changed" most often lands.
	// Healing already lifted them into the interrupted call's result.
	h.flushAsides()
}

// healOrphanToolCalls answers every persisted tool call that has no result with
// one that says the call was interrupted, and returns how many it wrote.
//
// The result is deliberately explicit about what is NOT known: an interrupted
// exec or edit may well have taken effect, so the model is told to CHECK rather
// than to assume either way. Anything buffered is lifted into the last result, so
// the model reads the interruption and the user's follow-up in the same breath.
func (h *Harness) healOrphanToolCalls(ctx context.Context) int {
	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		return 0
	}
	answered := map[string]bool{}
	for _, e := range entries {
		if e.Kind == agent.KindToolResult && e.ToolCallID != "" {
			answered[e.ToolCallID] = true
		}
	}
	var orphans []agent.Entry
	for _, e := range entries {
		if e.Kind == agent.KindToolCall && e.ToolCallID != "" && !answered[e.ToolCallID] {
			orphans = append(orphans, e)
		}
	}
	for i, o := range orphans {
		name := o.ToolName
		if name == "" {
			name = "the tool"
		}
		content := fmt.Sprintf(
			"ERROR: this %s call was INTERRUPTED — the connection to the model dropped before "+
				"the result was recorded. Whether it took effect is unknown: verify the current "+
				"state (re-read the file, re-run the check) before repeating it.", name)
		if i == len(orphans)-1 {
			content = h.liftQueued(content)
		}
		h.store.publish(agent.Entry{
			ID: uuid.New().String(), Kind: agent.KindToolResult,
			Content: content, ToolCallID: o.ToolCallID, ToolName: o.ToolName,
			CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(orphans)
}

// startBackground runs command asynchronously via backend (a container when
// DockerExec); on completion it injects a completion notification and wakes the
// driver. Returns the job id.
func (h *Harness) startBackground(backend ExecBackend, command string) int {
	h.bgMu.Lock()
	h.bgSeq++
	id := h.bgSeq
	h.bgRun++
	h.bgMu.Unlock()
	go func() {
		out := strings.TrimSpace(backend.Run(context.Background(), command))
		h.bgMu.Lock()
		h.bgRun--
		h.bgMu.Unlock()
		h.Notify(fmt.Sprintf("background job #%d finished — `%s`:\n%s", id, command, out))
		select {
		case h.wake <- struct{}{}:
		default: // wake is buffered; a full buffer just means a turn is already due
		}
	}()
	return id
}

// Start spawns the servers, waits for tool discovery, and builds the Session.
func Start(ctx context.Context, cfg Config) (*Harness, error) {
	// Explicit Go-level Servers win outright — a caller that constructed them
	// meant it. Otherwise resolve the layered config files, which fall back to
	// the built-in trio when neither exists.
	servers := cfg.Servers
	if servers == nil {
		dir := cfg.ConfigDir
		if dir == "" {
			dir = cfg.Workspace
		}
		var err error
		servers, err = LoadServers(dir, cfg.Workspace, cfg.RaglitHome)
		if err != nil {
			return nil, err
		}
	}
	for id, on := range cfg.AutostartOverride {
		for i := range servers {
			if servers[i].ID == id {
				servers[i].Autostart = on
			}
		}
	}
	mgr := mcpmgr.NewManager()
	store, err := openSessionStore(cfg.SessionFile)
	if err != nil {
		mgr.Close()
		return nil, fmt.Errorf("dun: open session: %w", err)
	}
	store.onNotify = cfg.OnNotify
	h := &Harness{mgr: mgr, store: store, client: cfg.Client,
		onRetry: cfg.OnRetry, wake: make(chan struct{}, 16),
		cfg: cfg, specs: servers, lastErr: map[string]string{}}

	// Autostart is best-effort by design: a missing binary or a server that
	// refuses to run is worth SAYING (it lands in Servers()[i].Err, which the
	// UI reports), but it is not worth refusing to start the session over. The
	// user can fix the binary and /rag on without losing the session.
	for _, s := range servers {
		if !s.Autostart {
			continue
		}
		if err := h.startServer(ctx, s); err != nil {
			log.Printf("dun: %s did not start: %v", s.ID, err)
		}
	}
	// Carry the client's own retry narration out to the UI. Without this the
	// waiting is only ever logged, and a TUI's log is not on screen.
	applyRetryPolicy(cfg.Client)
	wireRetry(cfg.Client, cfg.OnRetry)

	// Context shaping. The Shaper's algorithm is a LADDER, and every rung needs
	// its own policy field — setting BudgetTokens alone disables the cheap rungs
	// and leaves only the expensive one:
	//
	//   0. pristine tail   PreserveLastMessages / PreserveLastToolCalls
	//   1. LOD stubs       LODTruncateAboveChars  (render-time; no writes)
	//   2. compaction      runs ONLY if LOD still leaves it over budget
	//
	// Measured the hard way: a policy with only BudgetTokens set skipped rungs 0
	// and 1, so every build compacted and preserved nothing — 11 entries
	// survived from 97 tool calls, and rewriting the prefix each turn
	// invalidated the KV cache for 8.5x the processed tokens and 3x the wall
	// clock.
	//
	// LODTruncateAboveChars is the important one here: tool results ARE the
	// context in a coding agent (measured: 122k of a 180k window, one gradle log
	// 19.8k chars). Stubbing older oversized results is render-time and lossless
	// on disk, and it is what keeps compaction from ever being needed.
	shaper := &agent.Shaper{
		Store:  store,
		Runner: cfg.Client,
		Policy: agent.ShaperPolicy{
			BudgetTokens:          contextBudget(),
			VerbatimToolResults:   verbatimToolResults(),
			ToolFormat:            toolFormat(),
			LODTruncateAboveChars: 4000,
			PreserveLastMessages:  6,
			PreserveLastToolCalls: 4,
		},
	}
	h.Session = &agent.Session{
		SessionID:        "dun",
		Store:            store,
		Runner:           cfg.Client,
		OnAssistantToken: cfg.OnToken,
		MaxTurns:         maxTurns(),
		Build:            measuredBuild(shaper),
		ToolFormat:       toolFormat(),
	}
	// Tools, Dispatch, System and Preparer all depend on WHICH servers are
	// running, and that changes mid-session (/rag on, /lsp off). applyTools
	// owns those four fields; nothing else may set them.
	h.applyTools()
	return h, nil
}

// Ask injects a user message and runs the tool loop to completion.
//
// Sending a message is also how a user RESUMES a session that died on the
// provider: prepareTurn runs first, so an interrupted tool call is paired off
// before the new message is appended (appending it between a call and its result
// would be the one order the provider rejects), and the turn then retries the
// transport for as long as the policy allows.
func (h *Harness) Ask(ctx context.Context, task string) (agent.TurnResult, error) {
	h.prepareTurn(ctx)
	h.store.publish(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindUser, Content: task, CreatedAt: time.Now().UnixNano(),
	})
	return h.runTurn(ctx, h.Session.Turn)
}

// Close shuts down the MCP servers.
func (h *Harness) Close() { h.mgr.Close() }

// ToolNames lists the agent's tool names (MCP tools + the built-in exec), sorted.
func (h *Harness) ToolNames() []string {
	names := make([]string, len(h.Session.Tools))
	for i, t := range h.Session.Tools {
		names[i] = t.Function.Name
	}
	sort.Strings(names)
	return names
}


// dun's coding-agent persona + tool guidance.
//
// Assembled per session rather than fixed, because the tool families are not
// fixed: code and docs are opt-in and can come and go mid-session. Describing a
// family whose tools are absent is worse than saying nothing — the model plans
// around node_query, calls it, and gets "unknown tool".
const (
	systemPreamble = `You are dun, a coding agent working inside an isolated workspace.

Your tools:`
	systemCode  = "\n- code (poly-lsp-mcp): node_query to find/navigate code by selector (call it with selector \"?\" to learn the grammar), node_read to read a symbol whole, node_edit to edit/rename/refactor. Edits return diagnostics."
	systemShell = "\n- shell (mcpshell): eval runs sandboxed script code for computation, data wrangling, and jailed file ops; call the prompt tool for its language reference, help to list commands."
	systemDocs  = "\n- docs (raglit): search the document/knowledge index; ingest to add sources."
	systemExec  = "\n- exec: run a shell command (build/test/git/ls) in the workspace. Use it to VERIFY your edits — e.g. run the build and tests after changing code — and to run git."
	systemAsk   = "\n- ask_user: when the task is ambiguous or a decision is the user's to make (which approach, which file, is this OK to change), call ask_user with a clear question and optional options INSTEAD of guessing."

	systemDocsNote = "\n\nRelevant docs may be pushed to you as [docs] notes — use them."

	systemWorkCode = "\n\nWork step by step: find with node_query, read what you need, make minimal precise edits, verify via the diagnostics AND by running the build/tests with exec. Prefer node_edit over rewriting files. Be concise. When the task is done, briefly summarize what you changed."
	systemWork     = "\n\nWork step by step: read what you need before changing it, make minimal precise edits, and verify by running the build/tests with exec. Be concise. When the task is done, briefly summarize what you changed."
)

// systemFor describes only the tool families actually present. exec and ask_user
// are wired by the dispatcher, not MCP, so applyTools appends their lines when
// the corresponding Config hooks are set — here they are assumed, since every
// caller that has a workspace has exec.
func systemFor(tools []mcpmgr.MCPTool) string {
	have := map[string]bool{}
	for _, t := range tools {
		have[t.ServerID] = true
	}
	var b strings.Builder
	b.WriteString(systemPreamble)
	if have[ServerCode] {
		b.WriteString(systemCode)
	}
	if have[ServerShell] {
		b.WriteString(systemShell)
	}
	if have[ServerDocs] {
		b.WriteString(systemDocs)
	}
	b.WriteString(systemExec)
	b.WriteString(systemAsk)
	if have[ServerDocs] {
		b.WriteString(systemDocsNote)
	}
	if have[ServerCode] {
		b.WriteString(systemWorkCode)
	} else {
		b.WriteString(systemWork)
	}
	return b.String()
}

// maxTurns is the cap on agent loop iterations. 40 suits an interactive
// session, where the user is present and can nudge; a long autonomous task on a
// large repo can legitimately need far more, since paging through unfamiliar
// files costs turns before any edit happens. DUN_MAX_TURNS raises it.
func maxTurns() int {
	if v := os.Getenv("DUN_MAX_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 40
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

// toolFormat selects how tool calls travel, from DUN_TOOL_FORMAT.
//
// Native (default) uses the provider's tool_calls. "heredoc" carries calls as
// grammar-constrained text in ordinary content, parsed client-side.
//
// Reach for heredoc when the provider's own format loses data. Measured on Qwen3:
// a tool argument containing `</parameter>` comes back truncated at the
// delimiter, and the truncation is the MODEL's — visible with the server's parser
// bypassed — so no template or provider fix reaches it. It is also cheaper: 42%
// fewer prompt tokens and 8% fewer generated on a four-tool set.
//
// It REQUIRES a provider that accepts `grammar`, and llama.cpp refuses a grammar
// alongside `tools`, which is why this leaves the native path rather than
// extending it. Check first:  corrallm features <model>
func toolFormat() agent.ToolFormat {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUN_TOOL_FORMAT"))) {
	case "heredoc":
		log.Print("dun: DUN_TOOL_FORMAT=heredoc — tool calls travel as " +
			"grammar-constrained content, not the provider's tool_calls. Requires " +
			"grammar support; verify with `corrallm features <model>`.")
		return agent.ToolFormatHeredoc
	case "", "native":
		return agent.ToolFormatNative
	default:
		log.Printf("dun: unknown DUN_TOOL_FORMAT=%q, using native",
			os.Getenv("DUN_TOOL_FORMAT"))
		return agent.ToolFormatNative
	}
}

// verbatimToolResults passes tool-result content to the model byte for byte,
// including chat-template control tokens, from DUN_VERBATIM_TOOL_RESULTS.
//
// OFF by default, because a tool result is prompt, not data. dun reads files all
// day, and `<|im_start|>` in any of them — a log, a README, a document about the
// chat template — is a real control token (one token, not eleven characters), so
// a stock template renders it as a genuine turn boundary. Measured: the rendered
// turn sequence gains a SYSTEM message sourced from disk.
//
// Turn it on ONLY against a server whose template neutralizes those tokens
// itself, which is the better place for the fix: it costs nothing there and
// protects every client. ml-kit/templates/probe.py grades a template for exactly
// this; qwen3-hardened.jinja passes it.
func verbatimToolResults() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUN_VERBATIM_TOOL_RESULTS"))) {
	case "1", "true", "yes", "on":
		log.Print("dun: DUN_VERBATIM_TOOL_RESULTS is set — tool results go to the model " +
			"unescaped. This is only safe if the server's chat template neutralizes " +
			"control tokens; otherwise file contents can forge a turn.")
		return true
	}
	return false
}

// contextBudget is the token ceiling the Shaper shapes to, from
// DUN_CONTEXT_TOKENS (the model's window). 0 → no shaping at all, which is the
// old behaviour and the safe default: no probe can tell a 32k window from a
// 180k one without minutes of multi-megabyte requests, and guessing low is
// expensive — it compacts a large window for no reason.
//
// The fraction is deliberately close to 1. Shaping exists to stop a generation
// being cut off mid-write, not to keep the context small: the LOD rung already
// does that for free, and every compaction costs a prefix rewrite.
func contextBudget() int {
	v := os.Getenv("DUN_CONTEXT_TOKENS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("dun: ignoring invalid DUN_CONTEXT_TOKENS=%q", v)
		return 0
	}
	budget := n * 90 / 100
	log.Printf("dun: context window %d tokens; shaping budget %d (LOD stubs first, compaction last)", n, budget)
	return budget
}

// measuredBuild logs the size of the prompt the Shaper actually produced.
//
// Added because reasoning about context size from the SESSION FILE is
// unreliable: contents over 8 KiB are stored as blob references, so the JSONL
// reads ~187k chars for a conversation that materializes to ~715k. The only
// trustworthy number is the built message list, measured here.
//
// Logs the biggest message too: if a fold is not re-rooting the prompt, the
// giveaway is one surviving tool result still carrying tens of thousands of
// characters.
func measuredBuild(shaper *agent.Shaper) agent.ContextBuilder {
	return func(ctx context.Context, sessionID, system string) ([]llm.Message, error) {
		msgs, err := shaper.Build(ctx, sessionID, system)
		if err != nil {
			return msgs, err
		}
		total, biggest, biggestRole := 0, 0, ""
		for _, m := range msgs {
			n := len(m.Content)
			total += n
			if n > biggest {
				biggest, biggestRole = n, m.Role
			}
		}
		log.Printf("dun: built prompt: %d messages, %d chars (~%d tokens); largest %d chars (%s)",
			len(msgs), total, total/4, biggest, biggestRole)
		// DUN_DUMP_PROMPT writes the exact message list to disk so a failing
		// call can be replayed verbatim against the provider. The 500 that a
		// bad tool call produces carries no finish_reason, so the only way to
		// see one is to re-issue the identical request ourselves.
		if dir := os.Getenv("DUN_DUMP_PROMPT"); dir != "" {
			if b, err := json.Marshal(msgs); err == nil {
				_ = os.MkdirAll(dir, 0o755)
				_ = os.WriteFile(filepath.Join(dir,
					fmt.Sprintf("prompt-%d.json", time.Now().UnixNano())), b, 0o644)
			}
		}
		return msgs, nil
	}
}
