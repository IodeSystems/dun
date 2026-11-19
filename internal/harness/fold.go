package harness

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// What a fold is asked for (USER 2026-10-30: "500 tokens doesn't sound like
// much. It should be tuned to the pressure, and it should be with thinking
// off"). Both folds — agentkit's under budget pressure, and the rescue fold
// when the endpoint overflows (rescue.go) — size their summary from what they
// fold (agent.DefaultSummaryTokens), cap the completion to match, send
// thinking off, and are told what the session is for and what is in flight.

// foldOpts are a fold call's request options: thinking off, as /effort off
// sends it (Config.EffortOff for a model that cannot stop thinking). A
// summarizer that thinks spends its budget before it writes; on a server
// whose default thinks, that was thousands of tokens per fold.
//
// Off stays off when the fold carries the session's prefix (SessionPrefix),
// though the session's rounds may think: the summary's MaxTokens is sized
// to the summary, and thinking spends from the same cap. On OpenRouter and
// llama.cpp the reasoning field is not in the history's cached bytes, so the
// prefix still matches; on Anthropic a changed thinking setting drops the
// cached messages. Neither is measured yet. A model that refuses off
// folds at the lowest effort: agentkit resends it so (reasoningoff.go).
func (h *Harness) foldOpts() *llm.ChatOpts {
	o := &llm.ChatOpts{}
	h.applyEffort(o, EffortOff)
	return o
}

// foldTask is what a fold is told the session is for (agent.Shaper.TaskFunc):
// the plan and the work in flight, from dun's own records rather than from
// the conversation being folded, which is what loses them. Empty when there
// is neither: agentkit then uses the system prompt.
func (h *Harness) foldTask() string {
	var parts []string
	// The current ask, before anything else: a fold reads only the oldest
	// entries, and on a long tool-heavy turn the ask is one of them (the
	// pristine tail fills with tool entries). Also puts it on the plan.
	if a := h.noteAsk(context.Background()); a.text != "" {
		parts = append(parts, "# Current ask (copy it verbatim into the summary)\n"+a.block())
	}
	if b := h.planOpenSansAsk(); b != "" {
		parts = append(parts, "# Plan\n"+b)
	}
	if f := h.inFlight(); f != "" {
		parts = append(parts, "# In flight\n"+f)
	}
	return strings.Join(parts, "\n\n")
}

// inFlight is the work still running, one line each: background jobs not
// finished, and children not dismissed with their task.
func (h *Harness) inFlight() string {
	var lines []string
	for _, j := range h.bgJobList() {
		j.mu.Lock()
		done := j.done
		j.mu.Unlock()
		if !done {
			lines = append(lines, "- job "+j.status())
		}
	}
	for _, sa := range h.agentList() {
		if state, _, _, _ := sa.snapshot(); state == agentDismissed {
			continue
		}
		line := "- agent " + sa.line()
		if w := sa.where(); w != "" {
			line += " on " + w
		}
		lines = append(lines, line+" — task: "+oneLine(sa.prompt, 160))
	}
	return strings.Join(lines, "\n")
}

// foldRestoreCap is how much of a child's task, or a job's command, the
// after-fold notice quotes. The task is why the child exists; a one-line
// excerpt (inFlight's 160) is not enough to act on, and the whole of a long
// brief would cost every fold its size again.
const foldRestoreCap = 1000

// foldRestoreNote is what the model is told right after a fold: the work that
// is still alive, with each one's id, start time, how long it has run, and the
// command or task that started it. The summary may have lost all of it — the
// fold is TOLD what is in flight (foldTask), but nothing makes it write that
// down — and a model that does not know a job is running starts it again.
// Empty when nothing is in flight.
func (h *Harness) foldRestoreNote() string {
	now := time.Now()
	var head []string
	if a := h.askNow(); a.text != "" {
		head = append(head, "The current ask, verbatim:\n<ask>\n"+a.block()+"\n</ask>")
	}
	if b := h.asksBlock(); b != "" { // the asks ledger's (asktool.go)
		head = append(head, b)
	}
	// Without the rolling ask node: the ask is printed above, once.
	if b := h.planOpenSansAsk(); b != "" {
		head = append(head, "Open plan:\n"+b)
	}
	var lines []string
	for _, j := range h.bgJobList() {
		j.mu.Lock()
		done, started, written, command := j.done, j.started, j.written, j.command
		j.mu.Unlock()
		if done {
			continue
		}
		lines = append(lines, fmt.Sprintf("- job #%d — RUNNING now · started %s · running %s · %d bytes out — `%s`",
			j.id, started.Format("15:04:05"), roundDur(now.Sub(started)), written, oneLine(command, foldRestoreCap)))
	}
	for _, sa := range h.agentList() {
		state, status, _, since := sa.snapshot()
		if state == agentDismissed {
			continue
		}
		sa.mu.Lock()
		started, prompt := sa.started, sa.prompt
		sa.mu.Unlock()
		what := string(state)
		if state == agentRunning {
			what = "RUNNING now"
		}
		line := fmt.Sprintf("- agent #%d — %s", sa.num, what)
		if !started.IsZero() {
			// since is this run's length: still growing while running, fixed
			// once the run ended (snapshot) — so "ran", not "idle for".
			verb := "ran"
			if state == agentRunning {
				verb = "running"
			}
			line += fmt.Sprintf(" · started %s · %s %s", started.Format("15:04:05"), verb, roundDur(since))
		}
		if status != "" {
			line += " · " + status
		}
		lines = append(lines, line+" — task: "+oneLine(prompt, foldRestoreCap))
	}
	if len(lines) > 0 {
		head = append(head, "This work is still alive; do not start it again. "+
			"jobs(...) reports on any of it.\n"+strings.Join(lines, "\n"))
	}
	if len(head) == 0 {
		return ""
	}
	return "Context was just compacted; the summary may have dropped detail.\n" + strings.Join(head, "\n\n")
}

// ── the current ask ─────────────────────────────────────────────────────

// foldAskCap is how much of the ask the after-fold note quotes, and
// foldLaterCap how much of each correction. Past it the note says so; the
// plan node holds the whole text.
const (
	foldAskCap   = 8000
	foldLaterCap = 2000
)

// askTag marks the one plan node that holds the current ask.
const askTag = "[fold-ask]"

// foldAsk is the turn's ask: the latest user message, and what the person
// typed mid-turn after it (kindDelivered).
type foldAsk struct {
	id    string
	text  string
	at    int64 // CreatedAt, to tell a newer message from the cached one
	later []askLater
}

// askLater is a message typed while the agent worked.
type askLater struct {
	id   string
	text string
	at   int64
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + " …[truncated]"
}

// block is the ask as the model reads it: the first message, then each later
// one under "then, mid-turn:", each capped.
func (a foldAsk) block() string {
	b := capText(a.text, foldAskCap)
	for _, l := range a.later {
		b += "\n\nthen, mid-turn:\n" + capText(l.text, foldLaterCap)
	}
	return b
}

// full is the whole ask, uncapped, for the plan node.
func (a foldAsk) full() string {
	b := a.text
	for _, l := range a.later {
		b += "\n\nthen, mid-turn:\n" + l.text
	}
	return b
}

// askNow is the remembered ask, without reading the store.
func (h *Harness) askNow() foldAsk {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	a := h.ask
	a.later = append([]askLater(nil), a.later...)
	return a
}

// noteAsk finds the turn's ask and remembers it, and writes it to the plan's
// one ask node. Mechanical: no model call.
//
// Remembered, not read each time: once a fold has subsumed the ask, the store
// no longer holds it, and the next fold of the same turn would look for it in
// vain. A user entry newer than the remembered one replaces it, and its
// corrections with it. Only agent.KindUser and kindDelivered count: asides,
// notices and the settle prompts are notifications, so they never read as the
// ask. A delivered message is never folded (Context leaves it out), so History
// still has it.
//
// A child has no ask of its own to protect: its brief is pinned (harness.go).
func (h *Harness) noteAsk(ctx context.Context) foldAsk {
	if h.isChild() || h.store == nil {
		return foldAsk{}
	}
	// Where asks are kept they hold the ask (asktool.go), and no node is
	// written here: the latest message was "continue" as often as not.
	if a, ok := h.asksFoldAsk(); ok {
		return a
	}
	entries := h.store.History()
	h.askMu.Lock()
	for _, e := range entries {
		if e.Kind != agent.KindUser || strings.TrimSpace(e.Content) == "" {
			continue
		}
		if e.CreatedAt > h.ask.at || (e.CreatedAt == h.ask.at && e.ID > h.ask.id) {
			h.ask = foldAsk{id: e.ID, text: e.Content, at: e.CreatedAt}
		}
	}
	if h.ask.text == "" {
		h.seedAskFromNode(entries)
	}
	if h.ask.text != "" {
		for _, e := range entries {
			if e.Kind != kindDelivered || strings.TrimSpace(e.Content) == "" || e.CreatedAt < h.ask.at {
				continue
			}
			seen := false
			for _, l := range h.ask.later {
				seen = seen || l.id == e.ID
			}
			if !seen && e.ID != h.ask.id {
				h.ask.later = append(h.ask.later, askLater{id: e.ID, text: e.Content, at: e.CreatedAt})
			}
		}
		sort.SliceStable(h.ask.later, func(i, j int) bool { return h.ask.later[i].at < h.ask.later[j].at })
	}
	h.askMu.Unlock()
	a := h.askNow()
	if a.text != "" {
		h.planAsk(ctx, a)
	}
	return a
}

// seedAskFromNode restores the ask after a resume: the store holds no user
// entry for it once a fold took it, but the ask node's details still do. Its
// text splits back into the first message and the mid-turn ones. Everything
// in the store now is already in the node, so only delivered messages newer
// than the store's latest entry are added after this. Called with askMu held.
func (h *Harness) seedAskFromNode(entries []agent.Entry) {
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return
	}
	n := askNode(&g)
	if n == nil {
		return
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n.Details), askTag))
	parts := strings.Split(body, "\n\nthen, mid-turn:\n")
	if strings.TrimSpace(parts[0]) == "" {
		return
	}
	var at int64
	for _, e := range entries {
		at = max(at, e.CreatedAt)
	}
	h.ask = foldAsk{id: "node-" + n.ID, text: parts[0], at: at}
	for i, l := range parts[1:] {
		h.ask.later = append(h.ask.later, askLater{id: "node-" + n.ID + "-" + strconv.Itoa(i), text: l, at: at})
	}
}

// askNode is the open plan node holding the ask, nil when there is none.
func askNode(g *taskgraph.Graph) *taskgraph.Node {
	for _, n := range g.Nodes {
		if planOpen(n) && strings.Contains(n.Details, askTag) {
			return n
		}
	}
	return nil
}

// planAsk writes the ask to the plan's one ask node, in progress: made when
// there is none open, rewritten when the ask changed. One per session, not
// one per ask. A node the model or the person closed is left closed, and the
// next ask makes a new one: at most one is ever open.
func (h *Harness) planAsk(ctx context.Context, a foldAsk) {
	details := strings.TrimSpace(a.full() + "\n\n" + askTag)
	title := "Current ask: " + oneLine(a.text, 100)
	err := planStore{h}.Update(ctx, func(g *taskgraph.Graph) error {
		if n := askNode(g); n != nil {
			if n.Details == details && n.Title == title {
				return nil
			}
			doing := taskgraph.Doing
			p := taskgraph.Patch{Title: &title, Details: &details}
			if n.Status == taskgraph.Todo {
				p.Status = &doing
			}
			return g.Update(n.ID, p)
		}
		n, err := g.Add(taskgraph.Node{
			Title:    title,
			Details:  details,
			DoneWhen: "the ask is answered or carried out",
		})
		if err != nil {
			return err
		}
		return g.SetStatus(n.ID, taskgraph.Doing, "")
	})
	if err != nil {
		log.Printf("dun: fold: ask not written to the plan: %v", err)
	}
}

// clearAsk forgets the ask and drops its plan node: the conversation it was
// asked in is gone (Reset).
func (h *Harness) clearAsk() {
	h.askMu.Lock()
	h.ask = foldAsk{}
	h.askMu.Unlock()
	_ = planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		if n := askNode(g); n != nil {
			return g.SetStatus(n.ID, taskgraph.Dropped, "the conversation was cleared")
		}
		return nil
	})
}

// planOpenSansAsk is planOpen without the ask node, for text that prints the
// ask itself.
func (h *Harness) planOpenSansAsk() string {
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return ""
	}
	var keep []*taskgraph.Node
	for _, n := range g.Nodes {
		if !strings.Contains(n.Details, askTag) {
			keep = append(keep, n)
		}
	}
	g.Nodes = keep
	return g.Brief("")
}

// ── the summary: what it must hold, and the check ───────────────────────

// foldSections are the headings a fold summary must have, in this order.
var foldSections = []string{"## Ask", "## Done", "## Learned", "## Next"}

// foldSummaryPrompt is the fold's instructions (agent.Shaper.SummaryPrompt):
// the task, then the four sections, then the size.
func foldSummaryPrompt(task string, budget int) string {
	var b strings.Builder
	if task = strings.TrimSpace(task); task != "" {
		b.WriteString("The session these events belong to has this task:\n<task>\n" + task + "\n</task>\n\n")
	}
	fmt.Fprintf(&b, "Summarize the following events in about %d tokens, no more. Shorter is better: "+
		"every line must be something the work needs after the events are gone. "+
		"Write exactly these four sections, with these headings:\n"+
		"## Ask\nThe current ask, copied word for word from the task above (or from the user's latest request below if the task has none). Never paraphrase it. Add any mid-turn correction after it.\n"+
		"## Done\nWhat was changed, run or decided, one line each.\n"+
		"## Learned\nFacts, causes, decisions with their reason, approaches ruled out, and file:line, identifier and commit references.\n"+
		"## Next\nThe next concrete steps, in order, including work still running.\n"+
		"Drop tool output that bears on none of these. Output the summary only, no preamble.\n\n", budget)
	return b.String()
}

// foldSummaryProblems is what is wrong with a fold summary, nil when nothing:
// a section missing, or the ask not in it. The ask is matched on its first
// 300 characters with whitespace folded, so a summary that re-wraps it passes
// and one that paraphrases it does not. Later corrections are not required
// (foldRestoreNote carries them). With no ask, only the sections count.
func foldSummaryProblems(summary string, ask string) []string {
	var bad []string
	low := strings.ToLower(summary)
	for _, s := range foldSections {
		if !strings.Contains(low, strings.ToLower(s)) {
			bad = append(bad, "the section "+s+" is missing")
		}
	}
	if a := strings.Join(strings.Fields(ask), " "); a != "" {
		if r := []rune(a); len(r) > 300 {
			a = string(r[:300])
		}
		if !strings.Contains(strings.Join(strings.Fields(summary), " "), a) {
			bad = append(bad, "the ask is not copied word for word under ## Ask")
		}
	}
	return bad
}

// foldRunner wraps the Shaper's Runner, which carries only the fold calls.
// agentkit v0.29.0 gives the host no hook between the summary and its commit
// (OnCompactionComplete fires after Store.Compact), so the check sits here, on
// the call itself: a summary that fails is asked for once more, with the
// failure stated. A second failure is kept: foldRestoreNote carries the ask
// and the plan whatever the summary holds.
type foldRunner struct {
	inner agent.LLMRunner
	h     *Harness
}

// ChatStream checks a fold request; any other passes through. agent.IsFold
// tells them apart (agentkit v0.34.0): a fold's first request sends the
// session's own system, so the system prompt does not.
func (r foldRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if !agent.IsFold(ctx) {
		return r.inner.ChatStream(ctx, msgs, tools, opts)
	}
	ask := r.h.askNow().text
	text, errMsg, err := r.call(ctx, msgs, tools, opts)
	if err != nil || errMsg != "" {
		return replay(text, errMsg), err
	}
	// The Shaper sends a long fold in stages and the runner cannot tell the
	// last from the others (the call carries no marker), so every stage is
	// held to the full check; a stage that fails costs one more call.
	if first := foldSummaryProblems(text, ask); len(first) > 0 {
		log.Printf("dun: fold summary rejected (%s); asking once more", strings.Join(first, "; "))
		retry := append([]llm.Message(nil), msgs...)
		last := &retry[len(retry)-1]
		last.Content += "\n\nYour previous summary was rejected: " + strings.Join(first, "; ") +
			". Write it again with the four sections and the ask copied word for word."
		t2, e2, err2 := r.call(ctx, retry, tools, opts)
		switch {
		case err2 != nil || e2 != "":
			log.Printf("dun: fold summary retry failed (%v %s); keeping the first", err2, e2)
		case strings.TrimSpace(t2) == "":
			log.Printf("dun: fold summary retry was empty; keeping the first")
		default:
			// The retry stands only if it is better: it passes, or has
			// strictly fewer problems than the first.
			second := foldSummaryProblems(t2, ask)
			if len(second) == 0 || len(second) < len(first) {
				text = t2
			}
			if len(second) > 0 {
				log.Printf("dun: fold summary still rejected (%s); kept, the restore note covers it", strings.Join(second, "; "))
			}
		}
	}
	return replay(text, ""), nil
}

// sessionPrefix is the prefix a fold's first request sends ahead of the
// history (agent.Shaper.SessionPrefix, agentkit v0.33.1; a fold too big for
// one request since v0.34.0), so the provider serves the history from the
// session's cache; before, the fold read all of it uncached under the
// compaction worker's own prompt. system is the string
// agent.Session passes to Build: Turn's effective system, heredoc tool text
// and slot note included, the bytes the round sends. The tools are the
// round's (forkTools): the session's tools on the native transport, none on
// heredoc, where the schemas are in the system prompt. Set at every build
// (measuredBuild), so a change to the tools or the prompt (a server
// starting, /rag on) reaches the next fold.
func sessionPrefix(s *agent.Session, system string) agent.SessionPrefix {
	tools, _ := forkTools(s)
	return agent.SessionPrefix{System: system, Tools: tools}
}

// call is one summarizer call, measured as side-call kind "fold" (USER
// 2026-11-18): a shaper fold stage is up to two calls (the first and its
// retry), and neither showed in /usage or any spend view before.
func (r foldRunner) call(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (string, string, error) {
	start := time.Now()
	text, errMsg, usage, err := collectText(ctx, r.h.sideRunner("fold", r.inner), msgs, tools, opts)
	r.h.noteSideCall("fold", start, usage)
	return text, errMsg, err
}

// collectText reads one reply whole. After the finish it reads on to the
// close, as askOnce does: the usage can arrive after it, in a usage-only
// chunk, and usage is the last one any chunk carried (nil if none did).
func collectText(ctx context.Context, r agent.LLMRunner, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (text, errMsg string, usage *llm.Usage, err error) {
	ch, err := r.ChatStream(ctx, msgs, tools, opts)
	if err != nil {
		return "", "", nil, err
	}
	var b strings.Builder
	done := false
	for c := range ch {
		if c.Usage != nil {
			usage = c.Usage
		}
		if done {
			continue // past the finish only the usage counts
		}
		if c.Error != "" {
			errMsg = c.Error
			go func() {
				for range ch {
				}
			}()
			break
		}
		b.WriteString(c.Content)
		done = c.Done
	}
	return b.String(), errMsg, usage, nil
}

// replay is a finished reply as a stream.
func replay(text, errMsg string) <-chan llm.StreamChunk {
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: text, Error: errMsg, Done: true}
	close(ch)
	return ch
}

// foldWire sets the Shaper's fold knobs: thinking off, the task, the summary
// instructions, and the check.
func (h *Harness) foldWire(sh *agent.Shaper) {
	sh.SummaryOpts = h.foldOpts()
	sh.TaskFunc = h.foldTask
	sh.SummaryPrompt = foldSummaryPrompt
	sh.Runner = foldRunner{inner: sh.Runner, h: h}
}
