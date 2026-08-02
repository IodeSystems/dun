package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Recap — the model rewriting its own recent history.
//
// Compaction is the automatic answer to a full context window: a summarizer
// folds the oldest entries and the model has no say in it. Recap is the
// deliberate one. When a stretch of work went badly — a tool result nobody
// needed to keep, six attempts at the same fix, a misunderstanding that was
// corrected three turns later — the churn stays in the window forever, and
// every subsequent turn pays for it AND reads the wrong account of what
// happened.
//
// So the model replaces that stretch with what it now knows to be true. This is
// cheaper than compaction (no summarizer call), more accurate (the model
// writing it has the whole span in view), and it can fix the RECORD rather than
// merely shortening it — a misunderstanding removed here stops misleading every
// later turn.
//
// Three rules make it safe:
//
//   - It is CONFIRMED. Rewriting a conversation a human is part of is not the
//     model's decision to take alone, so the human sees the replacement and the
//     count before anything moves. A sub-agent has no human to ask and recaps
//     freely: its context is exactly what this is for and its transcript is not
//     the human's conversation.
//   - Nothing is DESTROYED. The subsumed entries move to a sidecar next to the
//     session file. They leave the context and the scrollback; they stay on disk,
//     because churn is the evidence you need to fix the tooling that produced it.
//   - Tool calls keep their PAIRS. A call whose result is dropped (or the
//     reverse) is the one shape a provider rejects outright, so the span is
//     checked before it is applied and refused if it would orphan one.

// kindRecap is the citation left in the transcript: which sidecar holds what
// was removed. Filtered out of Context, so it costs the model nothing and never
// renders as conversation — it exists for whoever is reading the log later.
const kindRecap agent.EntryKind = "recap"

// recapSpan is what a recap would do, computed before anything is applied.
type recapSpan struct {
	Subsumes []agent.Entry
	Kept     []agent.Entry // preserved inside the span (tool pairs named by keep)
	Anchor   *agent.Entry  // the entry the phrase matched; nil = start of session
	Chars    int
	// Unmatched are keep terms that named nothing. Reported rather than
	// ignored: a model that asked to keep a result and silently lost it has
	// been told the opposite of the truth.
	Unmatched []string
	KeptCalls []string // tool names kept, for the confirmation
}

// planRecap works out which entries a recap would replace.
//
// from is matched against entry content from the END backwards, so the phrase a
// model remembers most recently is the one it gets. Everything AFTER the match
// is in the span, except the tool call currently executing (this recap) — which
// must survive, or its own result is orphaned the moment it returns.
func planRecap(entries []agent.Entry, from string, keep []string) (recapSpan, error) {
	var sp recapSpan
	if strings.TrimSpace(from) == "" {
		return sp, fmt.Errorf("recap needs `from`: a phrase from the message the churn started after")
	}
	// Search BELOW the live call. The recap call's own arguments contain `from`
	// verbatim, so a plain backward search always matches the recap itself,
	// making every span empty. Found by the first end-to-end test.
	limit := len(entries)
	if live := liveCallID(entries); live != "" {
		for i, e := range entries {
			if e.ToolCallID == live && e.Kind == agent.KindToolCall {
				limit = i
				break
			}
		}
	}
	// Prefer a USER message. The nudge hands the model a phrase from the last
	// user message, and the model then echoes that phrase in its own assistant
	// turns — so "the last entry containing it" is an echo three lines back, and
	// the span collapses to nothing. Live: it recapped 1 entry of 85 characters
	// while a 255,720-character result sat untouched.
	start := -1
	for i := limit - 1; i >= 0; i-- {
		if entries[i].Kind == agent.KindUser && strings.Contains(entries[i].Content, from) {
			start = i
			break
		}
	}
	if start < 0 {
		for i := limit - 1; i >= 0; i-- {
			if strings.Contains(entries[i].Content, from) {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return sp, fmt.Errorf("no entry contains %q — quote a phrase exactly as it appears", from)
	}
	sp.Anchor = &entries[start]

	// A keep term may be a tool_call id, a tool NAME, or a substring of the
	// call's arguments — because the model cannot see call ids. They are
	// protocol-level and never appear in its context, so a keep list keyed only
	// on them is unusable by the only caller there is. Live run: the model
	// invented "exec_2", matched nothing, and silently lost the one result it
	// had asked to keep.
	keeping, matched := map[string]bool{}, map[string]bool{}
	live := liveCallID(entries)
	for _, term := range keep {
		if term == "" {
			continue
		}
		// A bare tool NAME keeps only that tool's MOST RECENT call. The model
		// reaches for the name it knows ("exec"), and keeping every call of it
		// preserved the 255,720-character `cat` the recap existed to remove —
		// measured, on the first run where the model recapped unprompted. A
		// phrase from the arguments is specific, so it keeps every match.
		byName := false
		for _, e := range entries[start+1:] {
			if e.Kind == agent.KindToolCall && e.ToolName == term && e.ToolCallID != live {
				byName = true
			}
		}
		for i := len(entries) - 1; i > start; i-- {
			e := entries[i]
			if e.Kind != agent.KindToolCall || e.ToolCallID == live {
				continue
			}
			hit := e.ToolCallID == term || strings.Contains(e.Content, term) ||
				(byName && e.ToolName == term)
			if !hit {
				continue
			}
			keeping[e.ToolCallID] = true
			matched[term] = true
			sp.KeptCalls = append(sp.KeptCalls, e.ToolName)
			if byName && e.ToolName == term && !strings.Contains(e.Content, term) {
				break // the most recent one only
			}
		}
	}
	for _, term := range keep {
		if term != "" && !matched[term] {
			sp.Unmatched = append(sp.Unmatched, term)
		}
	}
	// live (the tool call with no result yet: this recap) is resolved above.
	for _, e := range entries[start+1:] {
		switch {
		case e.Kind == kindRecap:
			continue // a previous citation; it is not conversation
		case e.ToolCallID != "" && (keeping[e.ToolCallID] || e.ToolCallID == live):
			sp.Kept = append(sp.Kept, e)
		case e.Kind == agent.KindAssistant && hasPendingCall(entries, e):
			// The assistant turn that ISSUED the live call: dropping it would
			// leave the call with no message to belong to.
			sp.Kept = append(sp.Kept, e)
		default:
			sp.Subsumes = append(sp.Subsumes, e)
			sp.Chars += len(e.Content)
		}
	}
	if len(sp.Subsumes) == 0 {
		return sp, fmt.Errorf("nothing to recap after %q — the span is already clean", from)
	}
	return sp, nil
}

// liveCallID is the tool call still awaiting a result: the recap call itself.
func liveCallID(entries []agent.Entry) string {
	results := map[string]bool{}
	for _, e := range entries {
		if e.Kind == agent.KindToolResult {
			results[e.ToolCallID] = true
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == agent.KindToolCall && !results[entries[i].ToolCallID] {
			return entries[i].ToolCallID
		}
	}
	return ""
}

// hasPendingCall reports whether e is an assistant entry immediately preceding
// an unanswered tool call.
func hasPendingCall(entries []agent.Entry, e agent.Entry) bool {
	live := liveCallID(entries)
	if live == "" {
		return false
	}
	for i, x := range entries {
		if x.ToolCallID == live && x.Kind == agent.KindToolCall && i > 0 {
			return entries[i-1].ID == e.ID
		}
	}
	return false
}

// preview is what the human confirms: what goes, what stays, what replaces it.
func (sp recapSpan) preview(summary, userEdit string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Recap would remove %d entries (~%d characters) from the conversation.\n", len(sp.Subsumes), sp.Chars)
	if n := len(sp.KeptCalls); n > 0 {
		fmt.Fprintf(&b, "Keeping the %s call%s and its result.\n", strings.Join(sp.KeptCalls, ", "), plural(n))
	}
	if len(sp.Unmatched) > 0 {
		fmt.Fprintf(&b, "NOTE: %q matched nothing and will NOT be kept.\n", strings.Join(sp.Unmatched, ", "))
	}
	b.WriteString("\nReplaced with:\n" + indent(summary))
	if userEdit != "" && sp.Anchor != nil {
		b.WriteString("\n\nAnd your message becomes:\n" + indent(userEdit))
	}
	b.WriteString("\n\nWhat is removed is kept on disk; it just leaves the conversation.")
	return b.String()
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  │ " + l
	}
	return strings.Join(lines, "\n")
}

// applyRecap rewrites the store: the span's entries move to a sidecar, one
// assistant entry takes their place, and a citation records where they went.
func (h *Harness) applyRecap(sp recapSpan, summary, userEdit string) (string, error) {
	sidecar := h.nextRecapFile()
	if sidecar != "" {
		if err := writeSidecar(sidecar, sp.Subsumes); err != nil {
			// Refuse rather than proceed: losing the churn silently is the one
			// outcome this design exists to prevent.
			return "", fmt.Errorf("recap not applied — could not save what it would remove: %w", err)
		}
	}
	now := time.Now().UnixNano()
	replacement := agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindAssistant,
		Content: summary, CreatedAt: now,
	}
	citation := agent.Entry{
		ID: uuid.New().String(), Kind: kindRecap, CreatedAt: now + 1,
		Content: fmt.Sprintf("recap: %d entries (~%d chars) → %s", len(sp.Subsumes), sp.Chars, sidecarName(sidecar)),
	}
	anchorID := ""
	if userEdit != "" && sp.Anchor != nil {
		anchorID = sp.Anchor.ID
	}
	h.store.recap(sp.Subsumes, replacement, citation, anchorID, userEdit)
	return citation.Content, nil
}

func sidecarName(path string) string {
	if path == "" {
		return "(in-memory session — not saved)"
	}
	return filepath.Base(path)
}

// nextRecapFile numbers the sidecars beside the session file.
func (h *Harness) nextRecapFile() string {
	base := h.cfg.SessionFile
	if base == "" {
		return ""
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	for n := 1; ; n++ {
		p := fmt.Sprintf("%s.recap%d.jsonl", stem, n)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		if n > 999 {
			return ""
		}
	}
}

func writeSidecar(path string, entries []agent.Entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return f.Sync()
}

// ── the tool ────────────────────────────────────────────────────────

func recapToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "recap"
	td.Function.Description = "Replace a stretch of this conversation with a corrected account of it. Use it after " +
		"churn — a huge tool result you no longer need, several failed attempts at the same thing, a " +
		"misunderstanding that was cleared up later. `from` is a phrase from the message the churn started " +
		"AFTER (quoted exactly); everything since is replaced by `summary`, which should read as the account " +
		"you WISH the conversation had, including the correct conclusion. Name any tool calls whose results " +
		"still matter in `keep` and they survive verbatim. `user` rewrites the anchoring user message when it " +
		"was ambiguous and you now know what was meant. Nothing is deleted — it moves to a file — but the " +
		"tokens leave your context, so use this when a span is costing you more than it is worth."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"from":    map[string]any{"type": "string", "description": "exact phrase from the message the churn began after"},
			"summary": map[string]any{"type": "string", "description": "what actually happened and what is true now, written to stand alone"},
			"keep": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "which tool results to keep verbatim: a tool NAME (\"exec\") or a phrase from its arguments (\"grep -c\"). Everything else in the span goes.",
			},
			"user": map[string]any{"type": "string", "description": "optional: a clearer wording of the anchoring user message"},
		},
		"required": []string{"from", "summary"},
	}
	return td
}

func withRecap(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "recap" {
			return inner(ctx, tc)
		}
		var args struct {
			From    string   `json:"from"`
			Summary string   `json:"summary"`
			Keep    []string `json:"keep"`
			User    string   `json:"user"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := h.runRecap(ctx, args.From, args.Summary, args.Keep, args.User)
		if onCall != nil {
			onCall("recap", map[string]any{"from": args.From, "summary": args.Summary, "keep": args.Keep}, out)
		}
		return out, nil
	}
}

func (h *Harness) runRecap(ctx context.Context, from, summary string, keep []string, userEdit string) string {
	if strings.TrimSpace(summary) == "" {
		return "ERROR: recap needs a summary — the account that replaces what it removes."
	}
	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		return "ERROR: could not read the conversation: " + err.Error()
	}
	sp, err := planRecap(entries, from, keep)
	if err != nil {
		return "ERROR: " + err.Error()
	}

	// A sub-agent has nobody to ask, and its context is exactly what this is
	// for. A root rewriting a conversation a human is part of asks first.
	if h.cfg.Ask != nil {
		ans, err := h.cfg.Ask(ctx, sp.preview(summary, userEdit), []string{"apply the recap", "leave it alone"}, false)
		if err != nil {
			return "Recap not applied: " + err.Error()
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "apply") {
			return "Recap declined by the user — the conversation is unchanged. Their answer: " + ans
		}
	}

	note, err := h.applyRecap(sp, summary, userEdit)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if h.cfg.OnRecap != nil {
		h.cfg.OnRecap(RecapNote{Entries: len(sp.Subsumes), Chars: sp.Chars, Note: note})
	}
	out := fmt.Sprintf("Done — %s. Everything since %q now reads as the summary you gave; "+
		"the removed entries are on disk and out of your context. Continue from here.", note, from)
	if len(sp.Unmatched) > 0 {
		// Said plainly, because the model asked to keep something and did not
		// get it. Silence here is how it learns the wrong lesson.
		out += fmt.Sprintf("\nNOTE: %s matched no tool call in that span, so nothing was kept for it. "+
			"`keep` takes a tool NAME or a phrase from the call's arguments — call ids are not visible to you.",
			strings.Join(sp.Unmatched, ", "))
	}
	return out
}

// RecapNote is what the UI is told about a recap: enough for one dim line, and
// nothing that re-renders the churn it just removed.
type RecapNote struct {
	Entries int
	Chars   int
	Note    string
}

// ── when to suggest it ──────────────────────────────────────────────
//
// A prompt line was not enough. Measured live, with recap fully described in
// the system prompt: `cat big.log` (255,720 chars), a failed eval, two help
// lookups and two retries — 902,875 tokens billed, 65,127 active — and the
// model never called it. That is not a wording problem. The model ends its turn
// the moment it has the answer, and nothing in that moment is about its context.
//
// So the suggestion is EVENT-driven, arriving at the moment the churn is made
// rather than long afterwards, and it names the specific thing to remove.
// "Consider recapping" is advice; "the 255,720-character exec result you have
// already moved on from" is an instruction with a subject.
//
// Three triggers, in the order they are worth acting on:
//
//   1. REPETITION — the same tool several times in a row. That is flailing, and
//      the earlier attempts are superseded by definition: whatever they said,
//      the model did not accept it. This is the cheapest churn to remove and
//      the moment it is most obviously churn.
//   2. A SUPERSEDED LARGE RESULT — something big arrived and the model has
//      since done something else, which is the evidence that it has taken what
//      it needs. Nudging the moment it arrives would be telling it to discard
//      what it just asked for.
//   3. WINDOW SIZE — the fallback, for a window that got large without either
//      shape. Silent when no single entry dominates: that is compaction's
//      problem, and vague advice is exactly what already failed.

// largeResultChars is when one tool result is worth naming on its own. Roughly
// 5k tokens — enough that removing it is worth a tool call.
const largeResultChars = 20000

// repeatCalls is how many identical calls in a row read as flailing rather than
// as a plan.
const repeatCalls = 3

// recapNudgeTokens is the window size that earns the fallback reminder.
const recapNudgeTokens = 20000

// recapNudgeGrowth is how much the window must grow before saying it again.
const recapNudgeGrowth = 15000

// recapCue is a reason to recap, and what to say about it.
type recapCue struct {
	key    string // dedupe: the same cue is never raised twice
	detail string
}

// recapCueFor decides whether what just happened is worth a suggestion. Pure,
// so the decision is testable without a session: entries is the conversation up
// to and including the call that just ran, and tool/size describe its result.
func recapCueFor(entries []agent.Entry, tool string, size int) (recapCue, bool) {
	// 1. Flailing: this call plus the trailing run of the same tool.
	run := 1
	for i := len(entries) - 1; i >= 0 && run < repeatCalls+2; i-- {
		if entries[i].Kind != agent.KindToolCall {
			continue
		}
		if entries[i].ToolName != tool {
			break
		}
		run++
	}
	if run >= repeatCalls {
		return recapCue{
			key: fmt.Sprintf("repeat:%s:%d", tool, run),
			detail: fmt.Sprintf("you have called %s %d times in a row. If the earlier attempts are "+
				"superseded by what you know now, they are pure cost", tool, run),
		}, true
	}
	// 2. Something large that the model has already moved past — moving past it
	// is exactly what this call proves.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Kind != agent.KindToolResult || len(e.Content) < largeResultChars {
			continue
		}
		name := e.ToolName
		if name == "" {
			name = "a tool"
		}
		return recapCue{
			key: "big:" + e.ID,
			detail: fmt.Sprintf("the %d-character %s result earlier in this conversation is still in your "+
				"context, and you have moved on from it", len(e.Content), name),
		}, true
	}
	_ = size
	return recapCue{}, false
}

// watchRecap is called after every tool call: it raises at most one suggestion,
// and never the same one twice.
func (h *Harness) watchRecap(tool string, size int) {
	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		return
	}
	cue, ok := recapCueFor(entries, tool, size)
	if !ok || !h.claimCue(cue.key) {
		return
	}
	h.Aside(recapAdvice(cue.detail, lastUserPhrase(entries)))
}

// maybeNudgeRecap is the window-size fallback, for churn with no shape.
func (h *Harness) maybeNudgeRecap(active int) {
	if active < recapNudgeTokens {
		return
	}
	h.recapMu.Lock()
	if h.recapNudged > 0 && active < h.recapNudged+recapNudgeGrowth {
		h.recapMu.Unlock()
		return
	}
	h.recapNudged = active
	h.recapMu.Unlock()

	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil || len(entries) == 0 {
		return
	}
	big := biggestEntry(entries)
	if len(big.Content) < 4000 {
		return // nothing specific to point at; that is compaction's problem
	}
	what := fmt.Sprintf("your context is now ~%d tokens and the largest single item in it is a "+
		"%d-character result", active, len(big.Content))
	if big.ToolName != "" {
		what += " from " + big.ToolName
	}
	h.Aside(recapAdvice(what, lastUserPhrase(entries)))
}

// recapAdvice is the one wording, so every trigger says it the same way: the
// reason, then the call, pre-filled with an anchor the model can quote back.
func recapAdvice(detail, anchor string) string {
	return fmt.Sprintf("Context note — %s. If you no longer need it verbatim, remove it: "+
		"recap({from: %q, summary: \"…what happened and what is true now…\"}), keeping anything still "+
		"load-bearing with keep:[\"<tool name or a phrase from the call>\"]. Nothing is lost; it moves "+
		"to a file and leaves your context.", detail, anchor)
}

// claimCue reports whether this suggestion has not been made before.
func (h *Harness) claimCue(key string) bool {
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	if h.recapSeen == nil {
		h.recapSeen = map[string]bool{}
	}
	if h.recapSeen[key] {
		return false
	}
	h.recapSeen[key] = true
	return true
}

// withRecapWatch runs after every tool call, so a suggestion lands at the moment
// the churn is created rather than whenever the window happens to be measured.
func withRecapWatch(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		out, err := inner(ctx, tc)
		if err == nil && tc.Function.Name != "recap" {
			h.watchRecap(tc.Function.Name, len(out))
		}
		return out, err
	}
}

// biggestEntry is the single largest thing in the window — the one worth naming.
func biggestEntry(entries []agent.Entry) agent.Entry {
	var big agent.Entry
	for _, e := range entries {
		if len(e.Content) > len(big.Content) {
			big = e
		}
	}
	return big
}

// lastUserPhrase is an anchor the model can quote back: the opening of the most
// recent user message. Handing it one removes the commonest way `from` fails.
func lastUserPhrase(entries []agent.Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == agent.KindUser {
			words := strings.Fields(entries[i].Content)
			if len(words) > 8 {
				words = words[:8]
			}
			return strings.Join(words, " ")
		}
	}
	return ""
}
