package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
	"github.com/iodesystems/dun/internal/memory"
)

// learn.go — the learning phase, first slice: churn → resolved candidate →
// lesson (docs/strategies.md, "Learning phase").
//
// Episode: the plan nudge's failure streak (plannudge.go: nudge.fails, the
// one counter) of at least churnMinFails failed tool results, ended by a
// success on the target of one of the failed calls (churnTarget: the tool,
// and for a call with a path, its path). A streak ended by a success on
// another target, or never ended, produces nothing: lessons come from
// resolutions only (USER 2026-11-09).
//
// A stall episode (USER 2026-11-09): the stalled-plan nudge fired
// (planStallRound), then the main agent's plan moved (planMoved). It holds
// the open nodes, the rounds stalled, the calls before the nudge, and the
// move. Same cap and gate.
//
// Gate (USER 2026-11-09): episodes are held in the session until the task
// verifies, and only then is a lesson call made (learnVerified):
//   - the turn ended without an error, after an end-of-turn check in that
//     turn (idlecheck.go: the requirement checklist, or the open-nodes
//     note), with a plan of at least one done node and none open (done or
//     dropped: a dropped approach replaced by another is closed).
// Episodes that never verify are dropped with the session: they are held
// in memory only.
//
// Lesson: once verified (learnVerified), one side call per episode (kind
// "learn": no tools, no history, at most learnMaxTokens out, a prompt of at
// most churnKeep failed calls and the fix, each clipped) turns it into a
// candidate {name, detail, tags}. At most learnCap calls per session: the
// cap counts calls, not episodes held; at most learnHoldMax are held (the
// oldest go). The model may answer skip.
// The task's text is not in the prompt.
//
// Genericity (memory lesson.go: genericityError): deterministic heuristics reject a detail
// with a path, a file name, a URL, a hash or long number, quoted code, an
// identifier from the workspace or the episode, or the task's words. A
// lesson is for a different task: what is useless without this one is
// dropped, so a benchmark score measures retrieval, not memorization.
//
// The candidate is a strategy in the project's scope tagged candidate,
// written by strategyStore().Propose as the memory tool's add (file, dund
// ingest), with a "proposed" entry in its attestation log. It is not
// important: only the user marks that. Recall leaves candidates out
// (memory.Finder: a tag filter in dun, as raglit's filter keeps tags and cannot
// exclude one; raglit is asked for NotTags, USER 2026-11-09). A lesson that
// comes back as a current candidate adds a "recurred" entry to its log, the
// count /memories candidates shows; it is never accepted for the user.
// The user reviews them: /memories candidates, accept (the
// tag goes, a "confirmed" entry by the user), reject (a void by the user).
//
// Off without a strategies store (DUN_MEMORY=0 or no dund link; the Harbor
// adapter sets DUN_MEMORY=0), and in a child.

const (
	// churnMinFails failed results in a row, then a success on one of
	// their targets, is an episode.
	churnMinFails = 3
	// churnKeep is how many of the streak's failed calls the prompt holds:
	// the last ones.
	churnKeep = 6
	// churnArgsMax and churnOutMax clip a call's arguments and result, in
	// bytes, for the prompt.
	churnArgsMax = 240
	churnOutMax  = 320

	// learnCap is the lesson calls one session makes.
	learnCap = 3
	// learnMaxTokens caps one lesson call's answer.
	learnMaxTokens = 400
	// learnHoldMax is how many episodes wait for the task to verify.
	learnHoldMax = 12
	// learnTimeout bounds one lesson call.
	learnTimeout = 90 * time.Second
)

// churnAttempt is one tool call in brief.
type churnAttempt struct {
	Tool, Target, Args, Out string
}

// churnEpisode is a resolved streak: its length, its last failed calls
// (oldest first), and the call that worked.
//
// A stall episode has Stall (the rounds with no plan move) and Open (the
// open nodes the nudge named) instead of Fails; Tried is the calls before
// the nudge, and Fix the plan move.
type churnEpisode struct {
	Fails int
	Stall int
	Open  []string
	Tried []churnAttempt
	Fix   churnAttempt
}

// planMoveBrief says what a plan write moved: a node added, or its status,
// details, done_when or needs changed.
func planMoveBrief(cs []taskgraph.Change) string {
	var parts []string
	for _, c := range cs {
		if !planMove(c) {
			continue
		}
		a := c.After
		switch b := c.Before; {
		case b == nil:
			parts = append(parts, fmt.Sprintf("added %q (done when: %s)", a.Title, a.DoneWhen))
		case b.Status != a.Status:
			parts = append(parts, fmt.Sprintf("%q: %s → %s", a.Title, b.Status, a.Status))
		default:
			parts = append(parts, fmt.Sprintf("%q: details, done_when or needs changed", a.Title))
		}
	}
	return strings.Join(parts, "; ")
}

// newChurnAttempt is a call in brief: an exec's command, else a path, else
// the arguments; both clipped.
func newChurnAttempt(tool, args, out string) churnAttempt {
	brief := args
	var a struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) == nil {
		switch {
		case a.Command != "":
			brief = a.Command
		case a.Path != "" && len(args) > churnArgsMax:
			brief = a.Path
		}
	}
	return churnAttempt{Tool: tool, Target: churnTarget(tool, args), Args: clipMiddle(brief, churnArgsMax), Out: clipMiddle(out, churnOutMax)}
}

// churnTarget is what a call works on: the tool, and its path when it has
// one. An exec's target is exec: a different command that works is the fix.
func churnTarget(tool, args string) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) == nil && strings.TrimSpace(a.Path) != "" {
		return tool + ":" + strings.TrimSpace(a.Path)
	}
	return tool
}

// churnKeepLast appends a, keeping the last churnKeep.
func churnKeepLast(tried []churnAttempt, a churnAttempt) []churnAttempt {
	tried = append(tried, a)
	if len(tried) > churnKeep {
		tried = slices.Clone(tried[len(tried)-churnKeep:])
	}
	return tried
}

// churnResolved is the episode a success (fix) ends: a streak of fails, at
// least churnMinFails, with a failed call on fix's target. nil otherwise.
func churnResolved(fails int, tried []churnAttempt, fix churnAttempt) *churnEpisode {
	if fails < churnMinFails {
		return nil
	}
	if !slices.ContainsFunc(tried, func(t churnAttempt) bool { return t.Target == fix.Target }) {
		return nil
	}
	return &churnEpisode{Fails: fails, Tried: slices.Clone(tried), Fix: fix}
}

// clipMiddle keeps s's start and end, n bytes in all.
func clipMiddle(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	head := cutUTF8(s, n*2/3)
	tail := s[len(s)-(n-len(head)):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return head + " … " + tail
}

// learnState is the session's resolved episodes not yet turned into
// lessons, and the calls made.
type learnState struct {
	mu      sync.Mutex
	pending []churnEpisode
	calls   int        // lesson calls this session (learnCap); a cost cap, not reset on a fold (forget.go)
	run     sync.Mutex // one flush at a time
	wg      sync.WaitGroup
}

// learnOn reports whether this session learns: the main agent, with a
// strategies store.
func (h *Harness) learnOn() bool { return !h.isChild() && h.strategyStore() != nil }

// learnQueue holds a resolved episode until the task verifies, while the
// session has lesson calls left.
func (h *Harness) learnQueue(ep *churnEpisode) {
	if ep == nil || !h.learnOn() {
		return
	}
	h.learn.mu.Lock()
	defer h.learn.mu.Unlock()
	if h.learn.calls >= learnCap {
		return
	}
	h.learn.pending = append(h.learn.pending, *ep)
	if len(h.learn.pending) > learnHoldMax {
		h.learn.pending = slices.Clone(h.learn.pending[len(h.learn.pending)-learnHoldMax:])
	}
}

// learnAfterTurn releases the held episodes when the turn verified the
// task (taskVerified).
func (h *Harness) learnAfterTurn(ctx context.Context, err error) {
	if err == nil && h.taskVerified() {
		h.learnVerified(ctx)
	}
}

// taskVerified: an end-of-turn check was given in this turn, and the main
// agent's plan has a done node and none open.
func (h *Harness) taskVerified() bool {
	if !h.idle.wasChecked() {
		return false
	}
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return false
	}
	done := false
	for _, n := range g.Nodes {
		if planOpen(n) {
			return false
		}
		done = done || n.Status == taskgraph.Done
	}
	return done
}

// learnVerified turns the held episodes into candidates, in the
// background: the turn does not wait on it.
func (h *Harness) learnVerified(ctx context.Context) {
	h.learn.mu.Lock()
	n := len(h.learn.pending)
	h.learn.mu.Unlock()
	if n == 0 || !h.learnOn() {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), learnTimeout*learnCap)
	h.learn.wg.Add(1)
	go func() {
		defer RecoverSide("learn")
		defer h.learn.wg.Done()
		defer cancel()
		for _, out := range h.learnFlush(fctx) {
			log.Printf("dun: learn: %s", out)
		}
	}()
}

// learnFlush makes one lesson call per pending episode, up to the cap, and
// says what came of each.
func (h *Harness) learnFlush(ctx context.Context) []string {
	h.learn.run.Lock()
	defer h.learn.run.Unlock()
	var outs []string
	for {
		h.learn.mu.Lock()
		if len(h.learn.pending) == 0 || h.learn.calls >= learnCap {
			h.learn.pending = nil
			h.learn.mu.Unlock()
			return outs
		}
		ep := h.learn.pending[0]
		h.learn.pending = h.learn.pending[1:]
		h.learn.calls++
		h.learn.mu.Unlock()
		outs = append(outs, h.learnOne(ctx, ep))
	}
}

// learnOne is one episode's call, check and write.
func (h *Harness) learnOne(ctx context.Context, ep churnEpisode) string {
	s := h.strategyStore()
	if s == nil || h.Session == nil || h.Session.Runner == nil {
		return "off"
	}
	cctx, cancel := context.WithTimeout(ctx, learnTimeout)
	reply, err := h.learnCall(cctx, learnPrompt(ep))
	cancel()
	if err != nil {
		return "the lesson call failed: " + err.Error()
	}
	out, name := s.Propose(ctx, h.stratLink(), reply, memory.LessonSource{Task: h.learnTaskText,
		Calls: episodeText(ep), Workspace: h.cfg.Workspace, Note: episodeNote(ep), Meta: h.noteMeta})
	if name != "" && h.cfg.OnNotify != nil {
		h.cfg.OnNotify("learned a candidate strategy:" + name + " — /memories candidates to accept or reject it")
	}
	return out
}

// episodeNote is a candidate's proposed entry: where it came from.
func episodeNote(ep churnEpisode) string {
	if ep.Stall > 0 {
		return fmt.Sprintf("learning phase: the plan stalled %d rounds, then moved", ep.Stall)
	}
	return fmt.Sprintf("learning phase: %d failed calls in a row, then a %s call worked", ep.Fails, ep.Fix.Tool)
}

// learnInstruction is the lesson call's prompt head. Model-facing.
const learnInstruction = `A coding agent made several tool calls in a row that failed, then a call that worked. Write the lesson another agent could use on a DIFFERENT task: a general approach that would have avoided the failures, not this task's answer.

Rules:
- No file paths, file names, URLs, function, variable or project names, and no values from this task.
- No quoted code or output.
- The detail is at most 3 sentences.
- If the lesson is useless without this task, answer {"skip": true}.

Answer with one JSON object and nothing else:
{"name": "<2 to 5 words, kebab-case>", "detail": "<at most 3 sentences>", "tags": ["<1 to 3 short topic words>"]}
`

// learnStallInstruction is learnInstruction for a stall episode.
// Model-facing.
var learnStallInstruction = strings.Replace(learnInstruction,
	"made several tool calls in a row that failed, then a call that worked",
	"stopped making progress on its plan, then changed the plan and the task was finished", 1)

// learnPrompt is the episode in brief, after learnInstruction.
func learnPrompt(ep churnEpisode) string {
	var b strings.Builder
	if ep.Stall > 0 {
		b.WriteString(learnStallInstruction)
		fmt.Fprintf(&b, "\nThe plan did not move for %d rounds. Open steps: %s\n", ep.Stall, flatLine(strings.Join(ep.Open, "; ")))
		fmt.Fprintf(&b, "\nIts last calls before that (oldest first):\n")
		for i, t := range ep.Tried {
			fmt.Fprintf(&b, "%d. %s: %s\n   result: %s\n", i+1, t.Tool, flatLine(t.Args), flatLine(t.Out))
		}
		fmt.Fprintf(&b, "\nThen it changed the plan: %s\n", flatLine(ep.Fix.Args))
		return b.String()
	}
	b.WriteString(learnInstruction)
	fmt.Fprintf(&b, "\nFailed calls (%d in a row; the last %d, oldest first):\n", ep.Fails, len(ep.Tried))
	for i, t := range ep.Tried {
		fmt.Fprintf(&b, "%d. %s: %s\n   result: %s\n", i+1, t.Tool, flatLine(t.Args), flatLine(t.Out))
	}
	fmt.Fprintf(&b, "\nThe call that worked:\n%s: %s\n   result: %s\n", ep.Fix.Tool, flatLine(ep.Fix.Args), flatLine(ep.Fix.Out))
	return b.String()
}

func flatLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// learnCall asks the model once, outside the conversation.
func (h *Harness) learnCall(ctx context.Context, prompt string) (string, error) {
	start := time.Now()
	ch, err := h.sideRunner("learn", untee(h.Session.Runner)).ChatStream(ctx, []llm.Message{{Role: "user", Content: prompt}}, nil, &llm.ChatOpts{MaxTokens: learnMaxTokens})
	if err != nil {
		h.noteSideCall("learn", start, nil)
		return "", err
	}
	var out strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("learn", start, nil)
			return "", fmt.Errorf("%s", c.Error)
		}
		out.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("learn", start, usage)
	return out.String(), nil
}

// learnTaskText is the session's user messages: the task, for the filter.
func (h *Harness) learnTaskText() string {
	if h.store == nil {
		return ""
	}
	var b strings.Builder
	for _, e := range h.store.History() {
		if e.Kind == agent.KindUser {
			b.WriteString(e.Content + "\n")
		}
	}
	return b.String()
}

// episodeText is everything the episode's calls said.
func episodeText(ep churnEpisode) string {
	var b strings.Builder
	for _, t := range append(slices.Clone(ep.Tried), ep.Fix) {
		b.WriteString(t.Args + "\n" + t.Out + "\n")
	}
	return b.String()
}
