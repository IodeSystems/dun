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
	start := -1
	for i := limit - 1; i >= 0; i-- {
		if strings.Contains(entries[i].Content, from) {
			start = i
			break
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
	for _, e := range entries[start+1:] {
		// The live recap call is kept unconditionally, and its own arguments
		// QUOTE the keep terms — so matching it here made the confirmation read
		// "keeping the exec, recap calls", which is true and confusing.
		if e.Kind != agent.KindToolCall || e.ToolCallID == liveCallID(entries) {
			continue
		}
		for _, term := range keep {
			if term == "" {
				continue
			}
			if e.ToolCallID == term || e.ToolName == term || strings.Contains(e.Content, term) {
				keeping[e.ToolCallID] = true
				matched[term] = true
				sp.KeptCalls = append(sp.KeptCalls, e.ToolName)
			}
		}
	}
	for _, term := range keep {
		if term != "" && !matched[term] {
			sp.Unmatched = append(sp.Unmatched, term)
		}
	}
	// The live tool call is the LAST tool_call with no result yet: this one.
	live := liveCallID(entries)

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

// ── the nudge ───────────────────────────────────────────────────────
//
// A prompt line was not enough. Measured live, with recap fully described in
// the system prompt: `cat big.log` (255,720 chars), a failed eval, two help
// lookups and two retries — 902,875 tokens billed, 65,127 active — and the
// model never called it. That is not a wording problem. The model ends its turn
// the moment it has the answer, and nothing in that moment is about its context.
//
// So the reminder arrives WHEN it is expensive, and it names the specific entry
// that is costing the most. "Consider recapping" is advice; "the 255,720-char
// exec result from `cat big.log` is most of your window" is an instruction with
// a subject. It rides an Aside, so it never buys a turn of its own.

// recapNudgeTokens is the active-window size that earns a reminder. Below this
// the churn is not yet worth a tool call; a session that never crosses it is
// never nagged.
const recapNudgeTokens = 20000

// recapNudgeGrowth is how much the window must grow before saying it AGAIN.
// Without it, every chat round past the threshold repeats the same line.
const recapNudgeGrowth = 15000

// maybeNudgeRecap suggests a recap when the window is large, naming the biggest
// single entry in it. Called on every usage report; silent almost always.
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
	big, anchor := biggestEntry(entries), lastUserPhrase(entries)
	if big.Content == "" || len(big.Content) < 4000 {
		return // large window, no single offender: compaction's problem, not this
	}
	what := "a " + fmt.Sprintf("%d", len(big.Content)) + "-character result"
	if big.ToolName != "" {
		what += " from " + big.ToolName
	}
	msg := fmt.Sprintf("Your context is now ~%d tokens, and the largest single item in it is %s. "+
		"If you have already taken what you need from it, recap it away: "+
		"recap({from: %q, summary: \"…what happened and what is true now…\"}). "+
		"Keep anything still load-bearing with keep:[\"<tool name or a phrase from the call>\"].",
		active, what, anchor)
	h.Aside(msg)
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
