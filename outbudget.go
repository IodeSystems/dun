package dun

import (
	"log"
	"os"
	"strconv"

	"github.com/iodesystems/agentkit/llm"
)

// outbudget.go — the OUTPUT side of the context window.
//
// The window holds the prompt AND the response, and dun used to budget only the
// first. `logBudget` handed 90% of the window to the prompt, and nothing at all
// told the endpoint to stop generating: dun sends no `max_tokens`, llama.cpp's
// default `n_predict` is -1, and corrallm only clamps a value that is already
// there. With every cap unset, the only thing that ends a generation is the
// model choosing to stop — or the slot filling up.
//
// Measured, yscr 2026-08-24 09:54: window 188,160, one slot, prompt ~109k real
// tokens, and a tool call cut off after 1278 characters of arguments with
// finish_reason=length. Nothing was over-long. The response simply ran until
// prompt + generation reached 188,160, which took tens of thousands of tokens of
// thinking, because nothing anywhere had a number to compare against.
//
// So: reserve the response's room BEFORE shaping the prompt, and then actually
// SEND the reservation, in both dialects the endpoint understands —
// `max_tokens` for the whole response and `reasoning_budget_tokens` for the
// thinking inside it. A reserve that is not transmitted is a wish.

// defaultMaxOutputTokens caps ONE response.
//
// Sized from what a coding agent legitimately emits, not from what the window
// allows: the largest honest response is a file written through a heredoc tool
// call, and dun's own truncation detector already tells the model to split a
// write it cannot finish. 32k tokens is ~90k characters of output — far past any
// single call this agent makes, and far short of the 60-79k the runaway spent.
//
// It is also the RESERVE subtracted from the prompt budget, so raising it is not
// free: every token reserved for a response is a token the conversation cannot
// use. DUN_MAX_OUTPUT_TOKENS overrides.
const defaultMaxOutputTokens = 32768

// outputMargin is slack between the reservation and the wall, for the tokens
// neither side counts: the chat template's own scaffolding, a BOS, the couple of
// tokens a tokenizer disagrees with us about. Small, because the whole point of
// measuring the ratio is that this no longer has to absorb a 31% error.
const outputMargin = 2048

// minOutputTokens is the floor below which a response is not worth attempting.
//
// A budget under this means the prompt has eaten the window, and the honest
// answer is to compact rather than to ask for a reply in 500 tokens and get a
// truncated tool call — which is precisely the failure this file exists to
// prevent, reproduced deliberately.
const minOutputTokens = 4096

// maxOutputTokens is the hard cap on one response, from the environment or the
// default.
func maxOutputTokens() int {
	if v := os.Getenv("DUN_MAX_OUTPUT_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Printf("dun: ignoring invalid DUN_MAX_OUTPUT_TOKENS=%q", v)
		} else {
			return n
		}
	}
	return defaultMaxOutputTokens
}

// outputReserve is how much of the window is held back for the response: the cap
// plus the margin. This is what the prompt budget is window MINUS.
func outputReserve() int { return maxOutputTokens() + outputMargin }

// generationBudget sizes one response against what is actually left.
//
// promptTokens is this round's prompt, measured (see tokencal.go) rather than
// guessed. The cap is the smaller of the hard limit and the real room, so a
// prompt that grew past its budget — a pristine tail that cannot be shaped, a
// round the Shaper could not fold — still gets a request that fits instead of
// one that runs into the wall.
//
// ok=false means there is no honest budget to send: either the window is unknown
// (nothing to subtract from) or the room left is below minOutputTokens, which is
// the caller's cue to shed context instead of to ask for a tiny reply.
func generationBudget(window, promptTokens int) (int, bool) {
	if window <= 0 {
		return 0, false
	}
	room := window - promptTokens - outputMargin
	if room < minOutputTokens {
		return room, false
	}
	if cap := maxOutputTokens(); room > cap {
		room = cap
	}
	return room, true
}

// reasoningBudget splits a generation budget into thinking and answer.
//
// The fraction is agentkit's reasoningShare, which is Qwen3.8's own published
// ratio (262,144 reasoning against 131,072 response). Only the ratio survives
// being moved to a smaller window; the absolutes do not.
//
// Sized here rather than by llm.WithAutoReasoningBudget because that helper
// computes its free space as `window - CountPrompt(system, tools)` — it counts
// only the system prompt and the schemas as used, so against a 109k-token
// conversation it hands out a budget as if the window were empty. The number it
// needs is this round's whole prompt, which only the caller has.
func reasoningBudget(generation int) int { return generation * 2 / 3 }

// applyGeneration writes the round's caps onto the ChatOpts the next request
// will copy. Returns what it set, and ok=false when it set nothing.
//
// Mutating a shared *llm.ChatOpts is safe here and only here: agent.Session
// copies it by value at the top of every chat round, and the context builder
// that calls this runs on the turn's own goroutine immediately before that copy.
// Anything setting it from elsewhere would be a race.
func applyGeneration(opts *llm.ChatOpts, window, promptTokens int) (gen int, ok bool) {
	if opts == nil {
		return 0, false
	}
	gen, ok = generationBudget(window, promptTokens)
	if !ok {
		// Leave the previous round's caps in place rather than clearing them: an
		// unbounded request is exactly the failure mode being fixed, and a
		// too-small one is the caller's problem to solve by compacting.
		return gen, false
	}
	opts.MaxTokens = gen
	think := reasoningBudget(gen)
	opts.ReasoningBudgetTokens = &think
	opts.ReasoningBudgetMessage = llm.DefaultBudgetMessage
	return gen, true
}

// WindowBudget is how the endpoint's context window is divided, and how well
// that division is understood — the whole of it in one struct, because the
// numbers only mean anything together.
//
// It exists because /context could describe the pre-conversation cost in five
// itemised rows and could not say the one thing this session's work was about:
// how big the window is, how much of it the prompt may have, how much is held
// back for the answer, and whether any of those figures was MEASURED or is
// still the 4-chars-per-token default. A reader looking at a number has to be
// able to tell which.
type WindowBudget struct {
	// Window is the whole context, prompt and response together. 0 = nobody
	// stated one, which means no shaping and no generation cap — the state the
	// yscr session ran in, and the reason it is reported rather than hidden.
	Window int
	// Reserved is what is held back for the response: the cap plus the margin.
	Reserved int
	// Cap is the ceiling on one response on its own.
	Cap int
	// PromptBudget is what the Shaper shapes to: Window - Reserved - Overhead.
	PromptBudget int
	// Prompt is the last built prompt in tokens, overhead included.
	Prompt int
	// Overhead is the measured per-request cost our characters cannot see —
	// tool schemas, chat template. 0 until it has been fitted.
	Overhead int
	// CharsPerToken is the marginal ratio in force, and Measured says whether it
	// came from the provider or is still the default. Rounds is how many
	// observations it rests on, so one measurement is distinguishable from forty.
	CharsPerToken float64
	Measured      bool
	Rounds        int
	// Cuts is how many rounds this session the endpoint ended for room, and
	// Folds how many of those were answered by shedding history rather than by
	// a hint. Both belong here rather than with the compaction counters: a cut
	// is the budget being WRONG, and this is the budget.
	Cuts  int
	Folds int
}

// Free is the room a response would have right now: the window less the last
// prompt. Negative is possible and is not clamped — a prompt that has overrun
// the window is exactly the thing worth showing.
func (b WindowBudget) Free() int {
	if b.Window <= 0 {
		return 0
	}
	return b.Window - b.Prompt
}

// Known reports whether there is a window to divide at all.
func (b WindowBudget) Known() bool { return b.Window > 0 }

// Window returns the current division of the context window.
func (h *Harness) Window() WindowBudget {
	window := h.windowTokens()
	b := WindowBudget{
		Window:        window,
		Reserved:      outputReserve(),
		Cap:           maxOutputTokens(),
		Prompt:        h.meter.lastPromptTokens(),
		Overhead:      h.meter.Overhead(),
		CharsPerToken: h.meter.CharsPerToken(),
		Measured:      h.meter.Measured(),
		Rounds:        h.meter.rounds(),
	}
	if window > 0 {
		b.PromptBudget = shapingBudget(window) - b.Overhead
	}
	h.noteMu.Lock()
	b.Cuts, b.Folds = h.overflowCuts, h.overflowFolds
	h.noteMu.Unlock()
	return b
}
