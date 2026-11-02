package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

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

// contextWindower is agentkit's "ask the server how big the window is"
// capability, named as an interface so this asserts on the CAPABILITY rather
// than on *llm.Client, and so a test can supply one.
//
// The window comes from the environment (DUN_CONTEXT_TOKENS) if it is set, and
// from the SERVER if it is not. 0 → no shaping at all, and that remains the
// answer when neither can say.
//
// This used to read the environment only, justified by "no probe can tell a 32k
// window from a 180k one without minutes of multi-megabyte requests". That was
// true of PROBING and is not true of asking: llama.cpp states the number at
// /props as default_generation_settings.n_ctx, and agentkit's ContextWindow
// reads it in one request — including through corrallm's /upstream form.
// Measured: Qwen3-6-27B-MPT answers 220160. Making somebody type a number the
// server will hand over is not a safe default, it is a chore with a wrong
// answer available.
//
// The distinction the warning below is really about is GUESSING versus ASKING.
// A guessed window that is too low compacts a large context for nothing; a
// stated one is a measurement. Unlike the tokenizer lookup, ContextWindow
// caches nothing, so a timeout here costs this session its shaping and poisons
// no later call — which is why a short bound is safe here and is not there.
//
// "0 → no shaping" was the INTENT here from the start, but agentkit read a zero
// budget as a budget of zero tokens, so the default path compacted on every
// single turn. Two real sessions: 45 folds in 29 minutes and 38 in 7, each fold
// an LLM call, leaving 5 and 7 surviving entries. Fixed in the Shaper (a
// non-positive budget is now explicitly unbudgeted); the note is here because
// this is where the 0 comes from and a future "sensible default" would walk
// straight back into it.
type contextWindower interface {
	ContextWindow(ctx context.Context) (int, bool)
}

// contextWindowTimeout bounds the one request. Short on purpose: it runs during
// Start, and an endpoint that will not answer promptly should cost this session
// its shaping, not its startup.
const contextWindowTimeout = 10 * time.Second

// windowRetries bounds the re-asking. Six, spread by doubling, spans a long
// session while costing an endpoint that will NEVER state a window at most six
// timeouts over its whole life.
const windowRetries = 6

// windowTokens is the whole context as currently known, 0 while nobody has
// stated one.
func (h *Harness) windowTokens() int { return int(h.window.Load()) }

// ensureWindow re-asks for the window when the session is running without one.
//
// Startup asking used to be one shot: a single request, 10 seconds, and a miss
// cost the session its shaping and its generation cap for the rest of its life
// — silently, behind one log line. That is not a hypothetical. A resumed session
// asked at a moment the model was asleep, got no answer, and then ran a 187,003-
// token prompt against a 188,160-token window with nothing armed to notice: the
// same failure the whole overflow ladder exists to prevent, one layer up. The
// endpoint answered in 38ms when asked again three minutes later.
//
// So the ask is retried from the context builder — the one place that runs
// before every request, on the turn's goroutine — and the window is adopted
// mid-session the moment it can be had. Both halves arm together, because both
// are computed from h.window a few lines below.
//
// The backoff is in BUILDS rather than seconds because a build is what a missed
// window costs: waiting a fixed minute is either idle time on a session mid-turn
// or a needless timeout on one that is idle. Doubling means a brief blip is
// recovered from almost at once and a permanently silent endpoint is asked six
// times and then left alone.
func (h *Harness) ensureWindow(ctx context.Context) {
	if h.windowRunner == nil {
		return // a window is known, or asking has been given up
	}
	if h.windowSkip > 0 {
		h.windowSkip--
		return
	}
	h.windowAsks++
	if n, src := h.askWindow(ctx, h.windowRunner); n > 0 {
		h.window.Store(int64(n))
		h.windowSrc = src
		h.windowRunner = nil
		log.Printf("dun: context window resolved mid-session (ask %d since startup) — shaping "+
			"and the generation cap are armed from this round on", h.windowAsks)
		logWindow(n, src)
		return
	}
	if h.windowAsks >= windowRetries {
		h.windowRunner = nil
		log.Printf("dun: gave up asking for the context window after %d attempts — this session "+
			"runs unshaped and uncapped; set DUN_CONTEXT_TOKENS to arm it", h.windowAsks)
		return
	}
	h.windowSkip = 1 << (h.windowAsks - 1) // 1, 2, 4, 8, 16 builds
}

// askWindow asks for the window and says WHERE the answer came from, or returns
// 0 when nobody could state one. Silent, because it is asked repeatedly: the
// callers below do the logging, once per outcome rather than once per attempt.
func (h *Harness) askWindow(ctx context.Context, runner any) (int, string) {
	if v := os.Getenv("DUN_CONTEXT_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Printf("dun: ignoring invalid DUN_CONTEXT_TOKENS=%q", v)
		} else {
			return n, "DUN_CONTEXT_TOKENS"
		}
	}
	if n := savedWindow(h.cfg.Endpoint); n > 0 {
		return n, "the window set for " + h.cfg.Endpoint.Model + " (/context config --max)"
	}
	if w, ok := runner.(contextWindower); ok {
		cctx, cancel := context.WithTimeout(ctx, contextWindowTimeout)
		defer cancel()
		if n, ok := w.ContextWindow(cctx); ok && n > 0 {
			return n, "the server's /props"
		}
	}
	cctx, cancel := context.WithTimeout(ctx, contextWindowTimeout)
	defer cancel()
	if n := listedWindow(cctx, h.cfg.Endpoint); n > 0 {
		return n, "the server's model list"
	}
	return 0, ""
}

// contextWindow is the startup ask: the whole window, prompt and response
// together. 0 = nobody could say YET, which leaves shaping and generation caps
// unavailable until ensureWindow gets an answer.
func (h *Harness) contextWindow(ctx context.Context, runner any) int {
	if n, src := h.askWindow(ctx, runner); n > 0 {
		h.windowSrc = src
		return logWindow(n, src)
	}
	log.Printf("dun: no context window known — none set for this model (/context config --max), " +
		"and the server states none; no shaping and no generation cap until it answers, which " +
		"is asked again on the next build")
	return 0
}

// shapingBudget turns a window into the prompt's share of it. Pure: it is
// recomputed on every build (the fitted overhead moves), so it must not log.
func shapingBudget(window int) int {
	if window <= 0 {
		return 0
	}
	budget := window - outputReserve()
	if budget <= 0 {
		// A window smaller than one response's reservation. Shaping to a
		// non-positive budget means "unbudgeted" to the Shaper, which is worse
		// than shaping to something tight, so fall back to half the window —
		// the generation cap still bounds the other half. logWindow says so.
		budget = window / 2
	}
	return budget
}

// logWindow records WHERE the number came from. A session that thrashes should
// say in its own logs whether it was working from somebody's environment or
// from the server, because those are fixed in different places.
func logWindow(n int, source string) int {
	log.Printf("dun: context window %d tokens (from %s); reserving %d for the response, "+
		"shaping the prompt to %d (LOD stubs first, compaction last)",
		n, source, outputReserve(), shapingBudget(n))
	if n-outputReserve() <= 0 {
		log.Printf("dun: that window is smaller than one response's reservation (%d) — "+
			"shaping the prompt to half of it instead; lower DUN_MAX_OUTPUT_TOKENS to give "+
			"the conversation more of it", outputReserve())
	}
	return n
}

// toolLOD maps each tool to the end of an old, oversized result its stub
// keeps. Keyed by the tool's real name: TestToolLOD_NamesAreRealTools fails if
// a tool is renamed out from under it, which would silently fall back to head.
func toolLOD() map[string]agent.LODKeep {
	return map[string]agent.LODKeep{
		"exec": agent.LODEnds,
	}
}

// shaperPolicy is dun's shaping policy for a window (0 = not yet known).
//
// Assembled field by field rather than from agent.PolicyFor, because PolicyFor
// returns an EMPTY policy for an unknown window, and dun builds its Shaper
// before the window has been asked for (it is re-budgeted on every build — see
// measuredBuild). An empty policy would drop the LOD rung for exactly the
// sessions that start without a window. The cost of assembling it by hand is
// the bug aw4 had twice in one day: a field nobody set. TestShaperPolicy_
// CoversPolicyFor fails when agentkit's PolicyFor sets a field this leaves at
// zero, so a new agentkit knob is noticed rather than missed.
func shaperPolicy(window int) agent.ShaperPolicy {
	return agent.ShaperPolicy{
		BudgetTokens:          shapingBudget(window),
		VerbatimToolResults:   verbatimToolResults(),
		ToolFormat:            toolFormat(),
		LODTruncateAboveChars: lodChars(), // /context config --tool-lod
		PreserveLastMessages:  6,
		PreserveLastToolCalls: 4,
		// One result, wherever it sits: LOD skips the preserved tail, so a huge
		// recent result is sent whole every round until it ages out. exec clips
		// at 20k itself (execInlineMax); node_read and MCP results did not.
		MaxToolResultChars: agent.DefaultMaxToolResultChars,
	}
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
func measuredBuild(shaper *agent.Shaper, h *Harness) agent.ContextBuilder {
	meter := &h.meter
	return func(ctx context.Context, sessionID, system string) ([]llm.Message, error) {
		// A session that started without a window keeps asking for one. Here
		// rather than at startup only, because this runs before every request
		// and an endpoint that could not answer at 16:49 answers at 16:52.
		h.ensureWindow(ctx)
		// Re-budget before every build. The prompt's share of the window is
		// fixed, but what the Shaper is allowed to fill it with is that share
		// MINUS the per-request overhead the provider charges and our character
		// count cannot see — tool schemas above all. The overhead is measured,
		// so it arrives a few rounds in and moves; a budget set once at startup
		// would be over by exactly that much, forever. See promptMeter.
		if window := h.windowTokens(); window > 0 {
			if b := shapingBudget(window) - meter.Overhead(); b > 0 {
				shaper.Policy.BudgetTokens = b
			}
		}
		shaper.Policy.LODTruncateAboveChars = lodChars() // /context config --tool-lod, live
		msgs, err := shaper.Build(ctx, sessionID, system)
		if err != nil {
			return msgs, err
		}
		h.cache.noteBuild(msgs, h.Session.Tools, time.Now())
		if tb, err := json.Marshal(h.Session.Tools); err == nil {
			meter.NoteToolChars(len(tb))
		}
		total, biggest, biggestRole := 0, 0, ""
		for _, m := range msgs {
			n := len(m.Content)
			total += n
			if n > biggest {
				biggest, biggestRole = n, m.Role
			}
		}
		// What this request will actually cost the window: the text, plus the
		// constant. Both are needed here because the generation budget below is
		// window MINUS this, and leaving the constant out of it hands the
		// response room that is already spent.
		est := meter.Overhead()
		for _, m := range msgs {
			est += meter.Estimate(m.Content)
		}
		// Hand the size to the meter BEFORE the round that will price it: the
		// provider's usage report arrives with no record of what was sent, so
		// the pairing is positional. This builder is the last thing to run
		// before streamChat, on the same goroutine, which is what makes that
		// safe. See tokencal.go.
		meter.NoteBuild(total, est)
		how := "estimated at 4 chars/token"
		if meter.Measured() {
			how = fmt.Sprintf("measured at %.2f chars/token", meter.CharsPerToken())
		}
		// Size the RESPONSE against the prompt that was just built, and send the
		// number. This is the only place both halves are known at once: the
		// window from startup, the prompt from the line above. See outbudget.go.
		window := h.windowTokens()
		gen, ok := applyGeneration(h.Session.ChatOpts, window, est)
		applyEffort(h.Session.ChatOpts, h.Effort()) // /effort: thinking on, off or the server's
		room := "no window known, response uncapped"
		if window > 0 {
			room = fmt.Sprintf("response capped at %d (%d left in the %d window)",
				gen, window-est, window)
			if !ok {
				room = fmt.Sprintf("ONLY %d left in the %d window — below the %d floor; "+
					"keeping the previous cap and leaving it to the overflow policy",
					window-est, window, minOutputTokens)
			}
		}
		log.Printf("dun: built prompt: %d messages, %d chars (~%d tokens, %s); largest %d chars (%s); %s",
			len(msgs), total, est, how, biggest, biggestRole, room)
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
