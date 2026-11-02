package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// contextStats accumulates context usage data across the session for /context.
type contextStats struct {
	// Money and limits as the gateway reported them (cmd/dun/spend.go). nil
	// until the first usage or spend event.
	spend *proto.Spend

	// Token usage (cumulative across all turns)
	totalTokens     int
	activeTokens    int // last reported active window size
	cachedTokens    int
	processedTokens int
	generatedTokens int
	turns           int

	// Compaction history
	compactions     int
	tokensCompacted int // total tokens saved by compaction

	// Recap history
	recaps          int
	entriesRecapped int
	charsRecapped   int

	// Tool result truncation (LOD)
	toolResults      int // total tool results seen
	resultsTruncated int // how many were LOD-truncated

	// How the window is divided, and how well that division is understood. See
	// dun.WindowBudget: /context used to itemise the pre-conversation cost in
	// five rows and be unable to say how big the window was, so a reader could
	// see WHAT was expensive and never whether it was close to the wall.
	window         int
	windowReserved int
	windowCap      int
	promptBudget   int
	promptTokens   int
	promptOverhead int
	charsPerToken  float64
	ratioMeasured  bool
	ratioRounds    int
	windowCuts     int
	windowFolds    int

	// System prompt + tool schemas. systemExact says whether these were counted
	// by the model's own tokenizer or estimated at ~4 chars/token — the two must
	// never render alike.
	systemTokens int
	systemExact  bool
	systemPrompt int
	systemShared int
	systemParts  []dun.ContextPart
	// The project rules' share of systemPrompt and their /rules mode ("" =
	// no rules).
	systemRules     int
	systemRulesMode string
	// systemFiles are the context files in systemRules, each estimated.
	systemFiles []dun.ContextPart

	// Out-of-band delivery: messages typed mid-turn that were queued into
	// a running turn rather than starting a new one.
	oobMessages int

	// Notifications delivered via the queue (smuggled into tool results
	// rather than triggering their own turn).
	notificationsSmuggled int

	// Tool calls injected by the host that appear as if the
	// model made them.
	forcedToolCalls int

	// Side calls — the LLM calls that are not the turn: /suggest predictions,
	// /rephrase rewrites, commit messages, rescue compactions. Per-kind running
	// totals carried on the usage event; the engine sends the whole map each
	// turn, so the read replaces rather than accumulates.
	sideCalls map[string]sideCallStats

	// Proactive RAG's cost, from the usage event's "surfacing" map (a session
	// total — the read replaces). nil until the first pass.
	surfacing map[string]float64

	// The prompt cache and prefix churn, from the usage event's "cache" map
	// (a session total — the read replaces). nil until the first build.
	cache map[string]any
}

// sideCallStats is one /context row: the per-kind totals for the calls that
// are not the conversation turn.
type sideCallStats struct {
	calls     int
	latencyMS int64
	lastMS    int64
	processed int
	cached    int
	generated int
	avgMS     int64
	tokPerS   float64
}

// showConfig appends the running session's LLM settings to the conversation.
// The wizard (`dun --setup`) is a terminal flow — bubbletea owns stdin here — so
// this is read-only + a pointer to it.
func (m *tuiModel) showConfig() {
	key := "(none)"
	if m.keySet {
		key = "set"
	}
	var b strings.Builder
	b.WriteString(stHeader.Render("LLM settings"))
	b.WriteString("\n  " + stDim.Render("provider: ") + m.provider)
	b.WriteString("\n  " + stDim.Render("url:   ") + m.url)
	b.WriteString("\n  " + stDim.Render("model: ") + stTool.Render(m.model))
	b.WriteString("\n  " + stDim.Render("key:   ") + key)
	b.WriteString("\n  " + stDim.Render("saved: ") + configPath())
	b.WriteString("\n" + stDim.Render("use /model to switch models, or `dun --setup` to reconfigure"))
	m.append(b.String())
}

// sideCallBlock renders the per-kind stats for the LLM calls that are not the
// conversation turn. An empty map means no side calls this session (or an
// engine that predates the accounting), and the section says nothing at all —
// a "0 calls" row would be a fact about the absence of facts.
//
// Each row names what the kind IS in plain words, because the kind string is
// an engine identifier and a reader of /context should not have to know which
// helper "rescue" names. Latency shows the average and the last call — the
// average hides a slow one, the last one hides a fast streak; the two
// disagreeing is the interesting case. Tokens are per-kind running totals:
// processed/cached is the prompt the call re-read, generated is what it wrote.
// ctxLabelW is the label column of /context's aligned rows.
const ctxLabelW = 22

// ctxRow is one aligned /context row: the label padded to its column, the
// value, and a dim note after it. Labels longer than the column are cut, so
// a long server name cannot push its number out of line.
func ctxRow(label, value, note string) string {
	if lipgloss.Width(label) > ctxLabelW-1 {
		label = textfmt.Clip(label, ctxLabelW-1)
	}
	pad := strings.Repeat(" ", ctxLabelW-lipgloss.Width(label))
	return "\n  " + stDim.Render(label+pad) + value + stDim.Render(note)
}

// ctxNum is a count for a /context row: grouped, right-aligned, so a column
// of numbers lines up on its last digit.
func ctxNum(n int) string { return fmt.Sprintf("%9s", numfmt.Grouped(n)) }
func sideCallBlock(s *contextStats) string {
	if len(s.sideCalls) == 0 {
		return ""
	}
	// Stable order: map iteration is random and a re-render of /context must
	// not shuffle its own rows.
	kinds := make([]string, 0, len(s.sideCalls))
	for k := range s.sideCalls {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	what := map[string]string{
		"suggest":  "next-message predictions (/suggest)",
		"rephrase": "prompt rewrites (/rephrase)",
		"commit":   "commit messages (/worktree commit)",
		"rescue":   "context rescue compaction",
		"repaint":  "bug report redaction (/repaint report)",
		"worklog":  "task accounts (/worklog write)",
	}

	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("side calls"))
	b.WriteString(stDim.Render(" — LLM calls that are not the turn"))
	for _, k := range kinds {
		r := s.sideCalls[k]
		label := k
		if w, ok := what[k]; ok {
			label = k + " (" + w + ")"
		}
		b.WriteString("\n  " + stDim.Render(numfmt.Commaf("%-12s", label)))
		b.WriteString(textfmt.Plural(r.calls, "call"))
		if r.calls > 0 {
			b.WriteString(stDim.Render(numfmt.Commaf(" · avg %d ms · last %d ms", r.avgMS, r.lastMS)))
		}
		tok := stDim.Render(numfmt.Commaf(" · %d proc / %d cached / %d gen", r.processed, r.cached, r.generated))
		if r.tokPerS > 0 {
			tok += numfmt.Commaf(" · %.0f tok/s", r.tokPerS)
		}
		b.WriteString(tok)
	}
	return b.String()
}

// windowBlock renders how the context window is divided.
//
// The section /context was missing, and the reason the failure it describes went
// unnoticed for a whole session: the old view could itemise the pre-conversation
// cost across five rows and could not say how big the window was, so a reader
// could see WHAT was expensive without ever seeing whether it was near the wall.
//
// Every figure here says where it came from. A token count that is measured and
// one that is the 4-chars-per-token default must never render alike — that
// equivalence is precisely what let a 31%-low estimate pass for a measurement
// until the endpoint refused to generate.
func windowBlock(s *contextStats) string {
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("window"))
	if s.window <= 0 {
		// Not an absence of data — a STATE, and the one the yscr session ran in.
		// With no window there is nothing to shape against and no cap to send,
		// so the model generates until the endpoint stops it.
		b.WriteString("\n  " + stDim.Render("size:       ") + "not stated" +
			stDim.Render(" — no shaping, and no cap on a response"))
		b.WriteString("\n  " + stDim.Render("set DUN_CONTEXT_TOKENS, or use a server that states one."))
		return b.String()
	}

	pct := func(n int) string { return numfmt.Commaf(" (%d%%)", n*100/s.window) }
	b.WriteString("\n  " + stDim.Render("size:       ") + numfmt.Commaf("%d tokens", s.window))
	if s.promptTokens > 0 {
		b.WriteString("\n  " + stDim.Render("prompt:     ") +
			numfmt.Commaf("%d of %d budget", s.promptTokens, s.promptBudget) + stDim.Render(pct(s.promptTokens)))
	} else {
		b.WriteString("\n  " + stDim.Render("prompt:     ") +
			numfmt.Commaf("%d budget", s.promptBudget) + stDim.Render(" (nothing built yet)"))
	}
	if s.promptOverhead > 0 {
		// Charged once per REQUEST and invisible in any message, which is why it
		// gets its own row rather than being folded into the prompt: it is the
		// term a single chars-per-token ratio smears across the conversation.
		b.WriteString("\n  " + stDim.Render("  overhead: ") + numfmt.Commaf("%d", s.promptOverhead) +
			stDim.Render(" (schemas + template, once)"))
	}
	b.WriteString("\n  " + stDim.Render("response:   ") + numfmt.Commaf("%d reserved", s.windowReserved) +
		stDim.Render(numfmt.Commaf(" (cap %d + margin)", s.windowCap)))
	if s.promptTokens > 0 {
		free := s.window - s.promptTokens
		row := "\n  " + stDim.Render("room now:   ") + numfmt.Commaf("%d", free)
		if free < s.windowReserved {
			// The prompt has grown into the response's reservation. This is the
			// state a cut comes out of, so it is called out rather than left for
			// the reader to subtract.
			row += stNote.Render("  ← below the reservation")
		}
		b.WriteString(row)
	}

	how := stDim.Render(numfmt.Commaf(" (default — nothing measured yet; %d rounds)", s.ratioRounds))
	if s.ratioMeasured {
		how = stDim.Render(numfmt.Commaf(" (measured over %d rounds)", s.ratioRounds))
	}
	b.WriteString("\n  " + stDim.Render("ratio:      ") + numfmt.Commaf("%.2f chars/token", s.charsPerToken) + how)

	if s.windowCuts > 0 {
		// A cut is the budget having been WRONG, so it belongs here and not with
		// the compaction counters.
		what, plural := numfmt.Commaf("%d", s.windowCuts), " replies stopped for room"
		if s.windowCuts == 1 {
			plural = " reply stopped for room"
		}
		if s.windowFolds > 0 {
			what += numfmt.Commaf(" (%d folded)", s.windowFolds)
		}
		b.WriteString("\n  " + stDim.Render("cut short:  ") + what + stDim.Render(plural))
	}
	b.WriteString("\n  " + stDim.Render("the size is the server's when it states one; DUN_CONTEXT_TOKENS overrides it (the startup log names the source)"))
	return b.String()
}

// showContext appends a detailed breakdown of context usage to the conversation.
func (m *tuiModel) showContext() {
	s := &m.ctxStats
	var b strings.Builder
	b.WriteString(stHeader.Render("context"))

	b.WriteString("\n\n  " + stHeader.Render("tokens"))
	b.WriteString("\n  " + stDim.Render("processed:  ") + numfmt.Commaf("%d", s.processedTokens))
	b.WriteString("\n  " + stDim.Render("generated:  ") + numfmt.Commaf("%d", s.generatedTokens))
	b.WriteString("\n  " + stDim.Render("cached:     ") + numfmt.Commaf("%d", s.cachedTokens))
	b.WriteString("\n  " + stDim.Render("total:      ") + numfmt.Commaf("%d", s.totalTokens))
	b.WriteString("\n  " + stDim.Render("active:     ") + numfmt.Commaf("%d", s.activeTokens) + stDim.Render(" (last turn's window)"))
	b.WriteString("\n  " + stDim.Render("turns:      ") + numfmt.Commaf("%d", s.turns))

	b.WriteString(sideCallBlock(s))
	b.WriteString(surfacingBlock(s))
	b.WriteString(cacheBlock(s))
	b.WriteString(costBlock(s, time.Now()))
	b.WriteString(windowBlock(s))

	b.WriteString("\n\n  " + stHeader.Render("conversation"))
	b.WriteString("\n  " + stDim.Render("blocks:     ") + numfmt.Commaf("%d", len(m.convo)))
	b.WriteString("\n  " + stDim.Render("tools:      ") + numfmt.Commaf("%d", len(m.tools)))

	b.WriteString("\n\n  " + stHeader.Render("context shaping"))
	if s.compactions > 0 {
		b.WriteString("\n  " + stDim.Render("compactions: ") + numfmt.Commaf("%d", s.compactions) + stDim.Render(" · saved ") + numfmt.Commaf("%d tokens", s.tokensCompacted))
	} else {
		b.WriteString("\n  " + stDim.Render("compactions: ") + "0")
	}
	if s.recaps > 0 {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + numfmt.Commaf("%d", s.recaps) + stDim.Render(" · ") + numfmt.Commaf("%d entries, %d chars → disk", s.entriesRecapped, s.charsRecapped))
	} else {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("tool results"))
	b.WriteString("\n  " + stDim.Render("total:      ") + numfmt.Commaf("%d", s.toolResults))
	if s.resultsTruncated > 0 {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + numfmt.Commaf("%d", s.resultsTruncated) + stDim.Render(" (LOD-capped at 4000 chars)"))
	} else {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("system"))
	if s.systemTokens > 0 {
		how := " (estimated)"
		if s.systemExact {
			how = " (counted by the model's tokenizer)"
		}
		b.WriteString(ctxRow("prompt + schemas", ctxNum(s.systemTokens), " tokens"+how))
		// The breakdown is the point: one number says the total is large, and
		// these rows say which part to go and fix.
		if s.systemPrompt > 0 {
			b.WriteString(ctxRow("  system prompt", ctxNum(s.systemPrompt), ""))
		}
		// Part of the system prompt, not beside it: the project rules are the
		// one piece of it a user can change (/rules), so they get a row.
		if s.systemRulesMode != "" {
			b.WriteString(ctxRow("    project rules", ctxNum(s.systemRules), " (/rules "+s.systemRulesMode+")"))
		}
		for _, p := range s.systemParts {
			b.WriteString(ctxRow("  "+p.Name, ctxNum(p.Tokens), ""))
		}
		// Cost no single row owns — the provider's tool-use preamble, charged
		// once for declaring any tool at all. Shown only when there is some, and
		// named rather than folded into a server's row, because it does not go
		// away by turning one server off.
		if s.systemShared > 0 {
			b.WriteString(ctxRow("  tool-use overhead", ctxNum(s.systemShared), " (provider, shared)"))
		}
		if s.systemExact && len(s.systemParts) > 1 {
			b.WriteString("\n    " + stDim.Render("per-row figures are marginal: what you would save by removing that row."))
		}
	} else {
		b.WriteString(ctxRow("prompt + schemas", "not reported", ""))
	}

	b.WriteString("\n\n  " + stHeader.Render("out-of-band"))
	b.WriteString("\n  " + stDim.Render("queued msgs:    ") + numfmt.Commaf("%d", s.oobMessages) + stDim.Render(" (delivered mid-turn)"))
	if s.notificationsSmuggled > 0 {
		b.WriteString("\n  " + stDim.Render("notifications:  ") + numfmt.Commaf("%d", s.notificationsSmuggled) + stDim.Render(" (smuggled via queue)"))
	}
	if s.forcedToolCalls > 0 {
		b.WriteString("\n  " + stDim.Render("forced calls:   ") + numfmt.Commaf("%d", s.forcedToolCalls) + stDim.Render(" (host-injected)"))
	}

	m.append(b.String())
}

// readSystemCost decodes the pre-conversation cost off an event carrying it —
// context_cost at startup, usage after a turn. Shared by both so the two can
// never drift into disagreeing about the same numbers.
func (s *contextStats) readSystemCost(ev map[string]any) {
	v := evNum(ev["system_tokens"])
	if v <= 0 {
		return
	}
	s.systemTokens = int(v)
	s.systemExact, _ = ev["system_exact"].(bool)
	s.systemPrompt = int(evNum(ev["system_prompt"]))
	s.systemShared = int(evNum(ev["system_shared"]))
	s.systemParts = evParts(ev["system_parts"])
	s.systemRules = int(evNum(ev["system_rules"]))
	s.systemRulesMode = str(ev["system_rules_mode"])
	s.systemFiles = evParts(ev["system_files"])
}

// readWindow decodes the window division off an event carrying it — context_cost
// at startup, usage after every turn. A window of 0 IS the message ("nobody
// stated one"), so unlike readSystemCost this does not bail on a zero.
func (s *contextStats) readWindow(ev map[string]any) {
	if _, ok := ev["window"]; !ok {
		return // an older event, or one from a path that does not carry it
	}
	s.window = int(evNum(ev["window"]))
	s.windowReserved = int(evNum(ev["window_reserved"]))
	s.windowCap = int(evNum(ev["window_cap"]))
	s.promptBudget = int(evNum(ev["prompt_budget"]))
	s.promptTokens = int(evNum(ev["prompt_tokens"]))
	s.promptOverhead = int(evNum(ev["prompt_overhead"]))
	s.charsPerToken = evNum(ev["chars_per_token"])
	s.ratioMeasured, _ = ev["ratio_measured"].(bool)
	s.ratioRounds = int(evNum(ev["ratio_rounds"]))
	s.windowCuts = int(evNum(ev["window_cuts"]))
	s.windowFolds = int(evNum(ev["window_folds"]))
}

// readSurfacing decodes proactive RAG's totals off a usage event. Like
// readSideCalls, a missing field keeps the last snapshot.
func (s *contextStats) readSurfacing(ev map[string]any) {
	v, ok := ev["surfacing"].(map[string]any)
	if !ok {
		return
	}
	s.surfacing = make(map[string]float64, len(v))
	for k, x := range v {
		s.surfacing[k] = evNum(x)
	}
}

// surfacingBlock is the /context section for proactive RAG. The carried line
// is the point of it: a surfaced entry is re-sent every round until a fold
// drops it, so "appended" understates the cost and "carried" is what it has
// actually been in prompt tokens. Empty before the first pass.
func surfacingBlock(s *contextStats) string {
	if s.surfacing == nil {
		return ""
	}
	f := func(k string) int { return int(s.surfacing[k]) }
	entries := func(n int) string {
		if n == 1 {
			return "1 entry"
		}
		return numfmt.Commaf("%d entries", n)
	}
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("surfacing"))
	b.WriteString(stDim.Render(" — proactive RAG added to the conversation"))
	b.WriteString("\n  " + stDim.Render("passes:     ") + numfmt.Commaf("%d", f("passes")) +
		stDim.Render(numfmt.Commaf(" · avg %d ms · last %d ms", f("avg_ms"), f("last_ms"))))
	b.WriteString("\n  " + stDim.Render("surfaced:   ") + numfmt.Commaf("%d of %d found", f("surfaced"), f("found")))
	b.WriteString("\n  " + stDim.Render("appended:   ") + numfmt.Commaf("%s · %d bytes · ~%d tok", entries(f("entries")), f("bytes"), f("tokens")))
	b.WriteString("\n  " + stDim.Render("in history: ") + numfmt.Commaf("%s · ~%d tok", entries(f("in_history_entries")), f("in_history")) +
		stDim.Render(" (re-sent every round)"))
	b.WriteString("\n  " + stDim.Render("carried:    ") + numfmt.Commaf("~%d tok", f("carried")) +
		stDim.Render(numfmt.Commaf(" over %s", textfmt.Plural(f("rounds"), "round"))))
	return b.String()
}

// readCache keeps the usage event's "cache" map; a missing field keeps the
// last snapshot.
func (s *contextStats) readCache(ev map[string]any) {
	if v, ok := ev["cache"].(map[string]any); ok {
		s.cache = v
	}
}

// cacheBlock is the /context section for the prompt cache. Rounds are split by
// the build that preceded them, because the two causes of a miss need opposite
// fixes: a REWRITE is dun's shaping (fold, tool change, LOD step); a low hit
// on an APPEND-ONLY round means the server lost the slot in between. Empty
// before the first build.
func cacheBlock(s *contextStats) string {
	if s.cache == nil {
		return ""
	}
	n := func(m map[string]any, k string) int { return int(evNum(m[k])) }
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("cache"))
	b.WriteString(stDim.Render(" — what the server re-read, by the build before each round"))
	b.WriteString("\n  " + stDim.Render("rounds:     ") + numfmt.Commaf("%d · %.0f%% hit", n(s.cache, "rounds"), evNum(s.cache["hit_pct"])) +
		stDim.Render(numfmt.Commaf(" · %d cached / %d processed", n(s.cache, "cached"), n(s.cache, "processed"))))
	classes, _ := s.cache["classes"].(map[string]any)
	rows := []struct{ key, label string }{
		{"first", "first"},
		{"back_to_back", "append <1s"},
		{"after_gap", "append, gap"},
		{"rewrite", "rewrite"},
	}
	for _, r := range rows {
		c, ok := classes[r.key].(map[string]any)
		if !ok {
			continue
		}
		b.WriteString("\n  " + stDim.Render(numfmt.Commaf("%-12s", r.label)) +
			numfmt.Commaf("%d · %.0f%% hit", n(c, "rounds"), evNum(c["hit_pct"])) +
			stDim.Render(numfmt.Commaf(" · %d processed", n(c, "processed"))))
	}
	b.WriteString("\n  " + stDim.Render("builds:     ") + numfmt.Commaf("%d · %d append-only", n(s.cache, "builds"), n(s.cache, "append_only")))
	causes := []string{"fold", "rehead", "recap", "tools", "other"}
	rw := 0
	for _, c := range causes {
		rw += n(s.cache, "rewrites_"+c)
	}
	if rw > 0 {
		var parts []string
		for _, c := range causes {
			parts = append(parts, numfmt.Commaf("%s %d", c, n(s.cache, "rewrites_"+c)))
		}
		b.WriteString("\n  " + stDim.Render("rewrites:   ") + numfmt.Commaf("%d", rw) +
			stDim.Render(numfmt.Commaf(" (%s) · %d chars re-sent", strings.Join(parts, " · "), n(s.cache, "resent_chars"))))
	}
	if rh := n(s.cache, "reheads"); rh > 0 {
		b.WriteString("\n  " + stDim.Render("re-headed:  ") + numfmt.Commaf("%d superseded reads", n(s.cache, "rehead_entries")) +
			stDim.Render(numfmt.Commaf(" · %d chars · in %d folds", n(s.cache, "rehead_chars"), rh)))
	}
	if last := n(s.cache, "last_chars"); last > 0 {
		b.WriteString("\n  " + stDim.Render("last build: ") + numfmt.Commaf("kept %d of %d chars", n(s.cache, "last_kept_chars"), last))
	}
	return b.String()
}

// readSideCalls decodes the per-kind side-call totals off a usage event. The
// engine sends the whole map (a session total, not a delta), so the read
// REPLACES the TUI's copy. An event without the field (an older engine, or a
// turn from before the first side call) leaves the previous snapshot in
// place: the last real numbers are still the best knowledge, and a section
// that blinks away on every turn is worse than one that lags.
func (s *contextStats) readSideCalls(ev map[string]any) {
	v, ok := ev["side_calls"].(map[string]any)
	if !ok {
		return
	}
	s.sideCalls = make(map[string]sideCallStats, len(v))
	for kind, row := range v {
		rm, ok := row.(map[string]any)
		if !ok {
			continue
		}
		s.sideCalls[kind] = sideCallStats{
			calls:     int(evNum(rm["calls"])),
			latencyMS: int64(evNum(rm["latency_ms"])),
			lastMS:    int64(evNum(rm["last_ms"])),
			processed: int(evNum(rm["processed"])),
			cached:    int(evNum(rm["cached"])),
			generated: int(evNum(rm["generated"])),
			avgMS:     int64(evNum(rm["avg_ms"])),
			tokPerS:   evNum(rm["tok_per_s"]),
		}
	}
}
