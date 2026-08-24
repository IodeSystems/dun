package dun

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// loopguard.go — the model calling the same tool with the same arguments, over
// and over, and nothing noticing.
//
// llm/repetition.go catches a loop INSIDE one generation: the same bytes
// re-emitted until something cuts the stream. This catches the loop one level
// up, across rounds — each response well-formed, each tool call valid, and the
// sequence going nowhere.
//
// WHAT THE MEASUREMENT SAYS, because it is not what was expected. Two candidate
// detectors were tried against a real stuck session (yscr, 2026-08-23):
//
//	near-identical arguments   REJECTED. The session's visible churn was six
//	                           attempts at one failing test, and consecutive
//	                           `exec` arguments scored 0.01–0.29 similarity —
//	                           each attempt was a genuinely different edit.
//	                           Syntactic similarity does not see it.
//	recurring result lines     REJECTED. Lines recurring across a sliding window
//	                           of 8 results: 3/8 in healthy regions, 4/8 in the
//	                           stuck one. A threshold cannot be put between those
//	                           without either missing the loop or refusing normal
//	                           debugging, where the same failure legitimately
//	                           recurs while you work on it.
//	identical arguments, CONSECUTIVE   ACCEPTED, and it found a loop nobody had
//	                           noticed: twelve back-to-back `recap` calls with
//	                           byte-identical arguments (calls 39–50). The first
//	                           folded 379 entries; the eleven after it each
//	                           folded ONE, writing recap17 through recap26 —
//	                           2,523 bytes apiece, all of them nothing.
//
// Consecutiveness is what makes it precise. The same session called `ship` with
// identical arguments 8 times and `git log …` 3 times, all legitimate, and all
// separated by gaps of 3 to 48 calls. Zero false positives at any threshold
// above 2, on the only evidence available.
//
// It is therefore a NARROW detector and that is deliberate. It catches "the
// model is stuck re-issuing one call". It does NOT catch semantic churn —
// six different edits chasing one failing test — and nothing here should be read
// as claiming otherwise. A detector that fires on normal iteration would be
// worse than none, because the thing it interrupts is someone working.

// defaultLoopRepeats is how many consecutive identical calls are refused at.
//
// 3, because the third adds nothing the second did not: two identical calls back
// to back, with no other tool call between them to change the world, have
// already returned the same answer twice. The observed loop ran to 12.
// DUN_LOOP_REPEATS overrides; 0 disables the guard.
const defaultLoopRepeats = 3

// loopAskAfter is how many further identical calls are tolerated before the
// human is brought in. The refusal is a message, and a model that reads the
// message and repeats the call anyway is not going to be argued out of it.
const loopAskAfter = 2

// loopResultHead is how much of the previous result to quote back. Enough to
// remind the model what it already got; not so much that a refusal costs as much
// context as the call would have.
const loopResultHead = 600

// pollingTools are the tools whose whole purpose is to be called again with the
// same arguments: "is that background job done yet", "what is the sub-agent
// doing", "the human has not answered". Repetition is their success mode, not
// their failure mode, and guarding them would break the only way dun waits for
// anything.
var pollingTools = map[string]bool{
	"exec_monitor":  true,
	"agent_monitor": true,
	"ask_user":      true,
}

// loopRepeats is the threshold in force.
func loopRepeats() int {
	if v := os.Getenv("DUN_LOOP_REPEATS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			log.Printf("dun: ignoring invalid DUN_LOOP_REPEATS=%q", v)
		} else {
			return n
		}
	}
	return defaultLoopRepeats
}

// loopGuard is the run of consecutive identical calls seen so far.
//
// One run, not a history: the moment a different call arrives the run is over,
// which is the whole definition being enforced. Guarded by its own mutex because
// a dispatcher is not documented to be single-threaded, even though today's tool
// loop is.
type loopGuard struct {
	mu     sync.Mutex
	tool   string
	key    string // normalized arguments
	n      int    // how many consecutive calls have matched
	last   string // what the last call that actually RAN returned
	asked  bool   // the escalation to the human has already fired for this run
	logged bool   // the run has already been logged, so it is not logged per call
}

// observe records a call and reports how many consecutive identical ones have
// now been seen, counting this one. A different tool or different arguments
// starts a new run at 1.
func (g *loopGuard) observe(tool, key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tool == tool && g.key == key {
		g.n++
		return g.n
	}
	g.tool, g.key, g.n = tool, key, 1
	g.last, g.asked, g.logged = "", false, false
	return 1
}

// noteResult remembers what a call that RAN returned, so a later refusal can
// quote it back. Refusals do not call this: the point is what the model already
// received, not what it was told about receiving it.
func (g *loopGuard) noteResult(result string) {
	g.mu.Lock()
	g.last = result
	g.mu.Unlock()
}

func (g *loopGuard) previous() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// claimAsk reports whether this run should escalate to the human, and marks it
// so the escalation happens once per run rather than on every further repeat.
func (g *loopGuard) claimAsk() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.asked {
		return false
	}
	g.asked = true
	return true
}

// claimLog is the same once-per-run gate for the log line.
func (g *loopGuard) claimLog() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.logged {
		return false
	}
	g.logged = true
	return true
}

// normalizeToolArgs canonicalises an argument string so that two calls that mean
// the same thing compare equal.
//
// Only two normalisations, both safe: JSON round-tripped through a map (Go
// marshals map keys in sorted order, so key order stops mattering) and outer
// whitespace trimmed. Nothing that could make DIFFERENT calls look identical —
// no case folding, no whitespace collapsing inside values, because the arguments
// being compared are shell commands and file contents, where a space is data.
func normalizeToolArgs(args string) string {
	s := strings.TrimSpace(args)
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// withLoopGuard refuses a tool call that has already been made, identically,
// loopRepeats() times in a row.
//
// REFUSES rather than warns. A note asking the model to stop is still a round
// trip that ends with the model deciding, and the observed loop ran twelve deep
// with every call succeeding — there was no failure for a warning to attach to.
// Not running the call is what actually breaks the cycle, and it costs nothing:
// the call could not have returned anything new, since nothing ran between it
// and the identical one before it.
//
// The refusal IS the delivery mechanism. It is a tool result, paired to the
// model's own call by construction, so it needs no notification, no aside and no
// place in the history to be dropped from later.
func withLoopGuard(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		limit := loopRepeats()
		name := tc.Function.Name
		if limit <= 0 || pollingTools[name] {
			// Polling tools still reset the run: a poll between two identical
			// calls means something was being waited on, which is exactly the
			// "something happened in between" this guard is looking for.
			h.loop.observe("", "")
			return inner(ctx, tc)
		}

		n := h.loop.observe(name, normalizeToolArgs(tc.Function.Arguments))
		if n < limit {
			out, err := inner(ctx, tc)
			if err == nil {
				h.loop.noteResult(out)
			}
			return out, err
		}

		if h.loop.claimLog() {
			log.Printf("dun: loop guard — %q called %d times in a row with identical arguments; "+
				"refusing further identical calls", name, n)
		}
		if n >= limit+loopAskAfter && h.cfg.Ask != nil && h.loop.claimAsk() {
			h.forceLoopAsk(name, n)
		}
		// Lift explicitly. This wrapper is OUTSIDE withLiftedQueue so that it
		// sees every tool, which means a refusal returns without the queue ever
		// being drained — and the queue is where a message the user typed
		// mid-turn is waiting. Stranding that behind a loop the user is probably
		// typing ABOUT is the worst possible moment to drop it.
		return h.liftQueued(loopRefusal(name, n, h.loop.previous())), nil
	}
}

// loopRefusal is what the model reads instead of the result it asked for.
//
// Says three things, in this order: that the call did not run, why running it
// could not have helped, and what to do instead. The middle one is load-bearing
// — a model told only "refused" treats it as a transport failure and retries,
// which is the loop.
func loopRefusal(tool string, n int, previous string) string {
	var b strings.Builder
	b.WriteString("ERROR: this call was NOT run. You have now called `")
	b.WriteString(tool)
	b.WriteString("` with byte-identical arguments ")
	b.WriteString(strconv.Itoa(n))
	b.WriteString(" times in a row, with no other tool call in between. Nothing has changed since " +
		"the last one, so this call cannot return anything the previous one did not.")
	if p := strings.TrimSpace(previous); p != "" {
		b.WriteString("\n\nWhat that call returned:\n")
		if len(p) > loopResultHead {
			p = p[:loopResultHead] + "\n…[truncated]"
		}
		b.WriteString(p)
	}
	b.WriteString("\n\nYou are stuck. Do something DIFFERENT: change the arguments, use a different " +
		"tool, re-read the state you are assuming, or stop and explain what you are trying to " +
		"achieve and what is blocking it. Repeating this call will keep being refused.")
	return b.String()
}

// forceLoopAsk hands the decision to the human, as a tool call the model appears
// to have made itself.
//
// ForceToolCall rather than a notification for the reason that mechanism exists:
// it is persisted as assistant(tool_calls) → tool(result), so the answer arrives
// as a proper result the model can act on, and the turn PAUSES on it. A
// notification would have to compete for attention with the loop it is
// interrupting.
func (h *Harness) forceLoopAsk(tool string, n int) {
	q := "I have called `" + tool + "` with the same arguments " + strconv.Itoa(n) +
		" times in a row and I am not making progress. How would you like me to proceed?"
	var tc llm.ToolCall
	tc.ID = "loopguard-" + strconv.Itoa(n)
	tc.Type = "function"
	tc.Function.Name = "ask_user"
	args, err := json.Marshal(map[string]any{
		"question": q,
		"options": []string{
			"Tell me what to try instead",
			"Stop and leave it where it is",
		},
	})
	if err != nil {
		return
	}
	tc.Function.Arguments = string(args)
	log.Printf("dun: loop guard — escalating to the user after %d identical %q calls", n, tool)
	h.ForceToolCall(tc)
}
