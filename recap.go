package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
//   - It is VISIBLE, not confirmed. Recap used to stop and ask, and the prompt
//     was the wrong shape for it: the human is asked to approve a rewrite of a
//     span they would have to re-read to judge, in the middle of a turn they
//     asked for, and the honest answer is almost always "yes, obviously". A
//     question whose answer is never no is a delay, not a safeguard. So it
//     applies and REPORTS: the count, the citation, and the replacement text,
//     all readable in the scrollback after the fact. The safeguard is that
//     nothing is destroyed, which is the next rule.
//   - Nothing is DESTROYED. The subsumed entries move to a sidecar next to the
//     session file. They leave the context and the scrollback; they stay on disk,
//     because churn is the evidence you need to fix the tooling that produced it.
//   - Tool calls keep their PAIRS. A call whose result is dropped (or the
//     reverse) is the one shape a provider rejects outright, so the span is
//     checked before it is applied and refused if it would orphan one.
//   - It stays inside the CURRENT TURN. The span starts at or after the latest
//     user message and never crosses one, and words the user typed mid-turn
//     (lifted into a tool result) are carried into the summary verbatim. The
//     user's words are the record of intent; a paraphrase of a correction is
//     how a correction gets lost. Earlier turns are finished work, and making
//     them smaller is compaction's job, run by policy, not the model's
//     mid-task judgment (USER 2026-10-28).

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
	// Said is what the user typed mid-turn inside the span (lifted into tool
	// results), in order, word for word: it goes into the summary unparaphrased.
	Said []string
	// Unmatched are keep terms that named nothing. Reported rather than
	// ignored: a model that asked to keep a result and silently lost it has
	// been told the opposite of the truth.
	Unmatched []string
	KeptCalls []string // tool names kept, for the confirmation
}

// planRecap works out which entries a recap would replace.
//
// The span is everything after the latest user message, or, with from, after
// the entry in this turn that from quotes — matched from the END backwards, so
// the phrase a model remembers most recently is the one it gets. It never
// reaches before the latest user message. The tool call currently executing
// (this recap) is never in it: its own result would be orphaned the moment it
// returns.
func planRecap(entries []agent.Entry, from string, keep []string) (recapSpan, error) {
	var sp recapSpan
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
	turn := turnStart(entries[:limit])
	start := turn
	if from = strings.TrimSpace(from); from != "" {
		// The user message first. A model quoting the user's words echoes them
		// in its own turns too, so "the last entry containing it" was an echo
		// three lines back and the span collapsed to nothing. Live: it recapped
		// 1 entry of 85 characters while a 255,720-character result sat untouched.
		start = -2
		if turn >= 0 && strings.Contains(entries[turn].Content, from) {
			start = turn
		} else {
			for i := limit - 1; i > turn; i-- {
				if strings.Contains(entries[i].Content, from) {
					start = i
					break
				}
			}
		}
		if start == -2 {
			for i := turn - 1; i >= 0; i-- {
				if strings.Contains(entries[i].Content, from) {
					return sp, fmt.Errorf("%q is before the user's latest message; recap covers only the "+
						"current turn — leave out `from` to start at the user's latest message", from)
				}
			}
			return sp, fmt.Errorf("no entry contains %q — quote a phrase exactly as it appears, or leave "+
				"out `from` to start at the user's latest message", from)
		}
	}
	if start >= 0 {
		sp.Anchor = &entries[start]
	}

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
			if e.Kind == agent.KindToolResult {
				sp.Said = append(sp.Said, liftedUserWords(e.Content)...)
			}
		}
	}
	if len(sp.Subsumes) == 0 {
		return sp, fmt.Errorf("nothing to recap in this turn — the span is already clean")
	}
	return sp, nil
}

// turnStart is the index of the latest user message: the current turn's start.
// -1 when there is none, so the turn is the whole session.
func turnStart(entries []agent.Entry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == agent.KindUser {
			return i
		}
	}
	return -1
}

// liftedUserWords finds what the user typed mid-turn in a tool result: liftQueued
// appends each buffered item after a blank line, under its kind's prefix. An
// item runs to the next lifted item or to the end.
func liftedUserWords(result string) []string {
	userPrefix := queuedPolicies[queuedUser].prefix
	var out []string
	for _, item := range liftedItems(result) {
		if t, ok := strings.CutPrefix(item, userPrefix); ok {
			out = append(out, t)
		}
	}
	return out
}

// liftedItems splits the lifted tail of a result into its items, prefix and all.
func liftedItems(result string) []string {
	starts := []int{}
	for _, p := range queuedPolicies {
		for off := 0; ; {
			i := strings.Index(result[off:], "\n\n"+p.prefix)
			if i < 0 {
				break
			}
			starts = append(starts, off+i+2)
			off += i + 2
		}
		if strings.HasPrefix(result, p.prefix) {
			starts = append(starts, 0) // lifted into an empty result
		}
	}
	sort.Ints(starts)
	starts = slices.Compact(starts)
	var out []string
	for k, a := range starts {
		b := len(result)
		if k+1 < len(starts) {
			b = starts[k+1] - 2
		}
		out = append(out, result[a:b])
	}
	return out
}

// withSaid is the summary as stored: the model's account, then the user's own
// words from the span, unparaphrased and in order.
func withSaid(summary string, said []string) string {
	if len(said) == 0 {
		return summary
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(summary, "\n"))
	b.WriteString("\n\nThe user said, during this span (verbatim):")
	for i, t := range said {
		fmt.Fprintf(&b, "\n%d. %q", i+1, t)
	}
	return b.String()
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

// report is what the human reads AFTER the fact: what went, what stayed, what
// replaced it. Nothing here is a question — the recap has already applied — so
// it is written as an account, and it carries the replacement text in full,
// because "9 entries removed" alone is a number, not visibility.
func (sp recapSpan) report(summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Removed %d entries (~%d characters) from the model's context.\n", len(sp.Subsumes), sp.Chars)
	if n := len(sp.KeptCalls); n > 0 {
		fmt.Fprintf(&b, "Kept the %s call%s and its result.\n", strings.Join(sp.KeptCalls, ", "), plural(n))
	}
	if len(sp.Unmatched) > 0 {
		fmt.Fprintf(&b, "NOTE: %q matched nothing and was NOT kept.\n", strings.Join(sp.Unmatched, ", "))
	}
	b.WriteString("\nReplaced with:\n" + indent(withSaid(summary, sp.Said)))
	b.WriteString("\n\nWhat was removed is on disk; it left the model's context, not the record.")
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
func (h *Harness) applyRecap(sp recapSpan, summary string) (string, error) {
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
		Content: withSaid(summary, sp.Said), CreatedAt: now,
	}
	citation := agent.Entry{
		ID: uuid.New().String(), Kind: kindRecap, CreatedAt: now + 1,
		Content: fmt.Sprintf("recap: %d entries (~%d chars) → %s", len(sp.Subsumes), sp.Chars, sidecarName(sidecar)),
	}
	h.store.recap(sp.Subsumes, replacement, citation)
	// Searchable after it leaves the context (raglitauth.go); a recap is
	// applied whether or not it is put.
	_ = agent.PutSpan(context.Background(), h.sessionIndex(), agent.SessionDocRecap,
		"recap-"+replacement.ID, "", summary, sp.Subsumes)
	h.cache.noteRecap()
	h.recapsSince.Add(1) // the next round's stat says a recap happened (roundstats.go)
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

// ── reading back what was clipped ───────────────────────────────────
//
// The cap saves the whole result to a file in the session's isolation tmp
// (isotmp.go) and the gap names its path: the same file on the host and,
// mounted at its own path, under --docker. It is read with file_read, which
// pages by byte offset (at) where there are no lines to grep. That was this
// file's read_saved tool, addressed by a ref because the host path did not
// exist inside the container (retired 2026-11-06: one tool fewer on every
// request; agentkit v0.25.0 gave file_read the paging).

// ── the tool ────────────────────────────────────────────────────────

// recap and read_saved were one tool, told apart by which fields a call
// filled. Every field was optional, so a call could fill neither shape
// ({from} alone), and a model that did so three times gave up on recap
// (USER 2026-10-27). Split, each states what it requires.

func recapToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "recap"
	// A recap at the end of a turn costs a request and pays back on every later
	// one, so it is worth it only when the conversation goes on: in 2 of 8
	// one-shot benchmark runs it cost 3-6 requests for nothing (USER 2026-10-27
	// benchmark); in a long conversation it is the turn's last chance, since a
	// span cannot reach back past a user message (USER 2026-10-28).
	td.Function.Description = "To make room for what comes next: replace a span of this " +
		"conversation with a summary once it stops being worth its context: a big result you have taken what " +
		"you need from, failed attempts before one worked, a misunderstanding cleared up later. Recap is not " +
		"recommended if you do not expect continued conversation. It covers this turn only, from the user's latest message; what the " +
		"user typed in it is kept word for word. The span is moved to a file, not deleted, but your summary is " +
		"all that stays in context, so make it accurate: do not round numbers or guess at what you did not read."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"from": map[string]any{"type": "string", "description": "only to start later than the user's latest " +
				"message: an exact phrase from the entry in this turn the span starts after"},
			"summary": map[string]any{"type": "string", "description": "what happened and what is true now, standing on its own"},
			"keep": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "results to keep verbatim, by tool name (\"exec\") or a phrase from the call (\"grep -c\"), not by id",
			},
		},
		"required": []string{"summary"},
	}
	return td
}

// readSavedRetired answers a call to the retired read_saved (an old
// transcript's habit, or recap given a ref): where saved output is now.
const readSavedRetired = "ERROR: read_saved is retired: saved output is a file now, and the note where a " +
	"result was clipped names its path. Read it with file_read (grep, a line range, or at for a byte page)."

func withRecap(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "recap" && tc.Function.Name != "read_saved" {
			return inner(ctx, tc)
		}
		var args struct {
			Ref     string   `json:"ref"`
			From    string   `json:"from"`
			Summary string   `json:"summary"`
			Keep    []string `json:"keep"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		// A ref, or read_saved at all, was a READ: answered with where saved
		// output is now, not run as a recap.
		var out string
		switch {
		case tc.Function.Name == "read_saved" || strings.TrimSpace(args.Ref) != "":
			out = readSavedRetired
		case strings.TrimSpace(args.Summary) == "":
			// Say the shape and what was sent, not one missing word (USER
			// 2026-10-27: three failed calls on "needs a summary", then the
			// model gave up on recap).
			var sent map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &sent)
			out = "ERROR: recap was called without summary.\n" +
				"recap {summary: string, from?: string, keep?: string[]} — summary is the account that " +
				"replaces this turn's work since the user's latest message; from starts the span later, after " +
				"an exact phrase from this turn; keep names tool results in the span to keep word for word. " +
				"To read a saved result, use file_read on the path its clip note names.\n" +
				"You sent " + sentSig(sent) + "."
		default:
			out = h.runRecap(ctx, args.From, args.Summary, args.Keep)
		}
		if onCall != nil {
			// Report what was actually ASKED. Read-mode calls used to display as
			// from:"" summary:"" — an empty rewrite — because only the rewrite
			// fields were passed on, so the UI showed a call nobody made.
			a := map[string]any{}
			if strings.TrimSpace(args.Ref) != "" {
				a["ref"] = args.Ref
			} else {
				a["from"], a["summary"], a["keep"] = args.From, args.Summary, args.Keep
			}
			onCall(tc.Function.Name, a, out)
		}
		return out, nil
	}
}

func (h *Harness) runRecap(ctx context.Context, from, summary string, keep []string) string {
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

	// No confirmation. Recap applies and then says what it did — see the rules
	// at the top of this file.
	note, err := h.applyRecap(sp, summary)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// The bar's context figure drops now: the removed entries' characters
	// at the measured ratio, less the summary that replaced them.
	h.req.adjustCtx(h.meter.Estimate(summary) - int(float64(sp.Chars)/h.meter.CharsPerToken()+0.5))
	h.workRecap(summary)
	if h.cfg.OnRecap != nil {
		h.cfg.OnRecap(RecapNote{
			Entries: len(sp.Subsumes), Chars: sp.Chars, Note: note,
			Detail: sp.report(summary),
		})
	}
	// Not "Continue from here": a recap made as the task's last step read that
	// as a reason to check the work again (USER 2026-10-27 benchmark).
	since := "the user's latest message"
	if strings.TrimSpace(from) != "" {
		since = fmt.Sprintf("%q", from)
	}
	out := fmt.Sprintf("Done — %s. Everything since %s now reads as the summary you gave; "+
		"the removed entries are on disk and out of your context. If the task is done, answer now; "+
		"otherwise continue.", note, since)
	if len(sp.Unmatched) > 0 {
		// Said plainly, because the model asked to keep something and did not
		// get it. Silence here is how it learns the wrong lesson.
		out += fmt.Sprintf("\nNOTE: %s matched no tool call in that span, so nothing was kept for it. "+
			"`keep` takes a tool NAME or a phrase from the call's arguments — call ids are not visible to you.",
			strings.Join(sp.Unmatched, ", "))
	}
	return out
}

// RecapNote is what the UI is told about a recap. Note is the one dim line;
// Detail is what it opens into — counts, what was kept, and the replacement
// text. Never the removed content itself: re-rendering the churn a recap just
// took out would defeat the point of it, and it is on disk for whoever wants it.
type RecapNote struct {
	Entries int
	Chars   int
	Note    string
	Detail  string
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

// repeatWorthRemoving is what that run must have COST before it is worth a tool
// call to remove. Below this the suggestion is noise — see recapCueFor.
const repeatWorthRemoving = 4000

// recapNudgeTokens is the window size that earns the fallback reminder, unless
// /context config --recap-nudge sets another (recapNudgeAt).
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
	// 1. Flailing: this call plus the trailing run of the same tool — but only
	// when the run is actually COSTING something.
	//
	// Found by dogfooding this on a real task: three node_query calls in a row
	// returned 44, 112 and 375 characters, the cue fired, and the model ignored
	// it. It was right to. Recapping a few hundred characters saves nothing and
	// costs a tool call, and a suggestion that is correct to ignore is training
	// the model to ignore suggestions. Repetition is the SHAPE of churn; size is
	// what makes it worth acting on.
	run, cost := 1, size
	for i := len(entries) - 1; i >= 0 && run < repeatCalls+2; i-- {
		e := entries[i]
		if e.Kind == agent.KindToolResult {
			cost += len(e.Content)
			continue
		}
		if e.Kind == agent.KindUser {
			break // recap cannot reach before it
		}
		if e.Kind != agent.KindToolCall {
			continue
		}
		if e.ToolName != tool {
			break
		}
		run++
	}
	if run >= repeatCalls && cost >= repeatWorthRemoving {
		return recapCue{
			key: fmt.Sprintf("repeat:%s:%d", tool, run),
			detail: fmt.Sprintf("you have called %s %d times in a row. If the earlier attempts are "+
				"superseded by what you know now, they are pure cost", tool, run),
		}, true
	}
	// 2. Something large that the model has already moved past — moving past it
	// is exactly what this call proves. In this turn: recap cannot reach before.
	for i := len(entries) - 1; i > turnStart(entries); i-- {
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
			detail: fmt.Sprintf("the %d-character %s result earlier in this turn is still in your "+
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
	h.Aside(recapAdvice(cue.detail))
}

// maybeNudgeRecap is the window-size fallback, for churn with no shape.
func (h *Harness) maybeNudgeRecap(active int) {
	if active < recapNudgeAt() { // /context config --recap-nudge
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
	big := biggestEntry(entries[turnStart(entries)+1:]) // recap cannot reach before the turn
	if len(big.Content) < 4000 {
		return // nothing specific to point at; that is compaction's problem
	}
	what := fmt.Sprintf("your context is now ~%d tokens and the largest single item in it is a "+
		"%d-character result", active, len(big.Content))
	if big.ToolName != "" {
		what += " from " + big.ToolName
	}
	h.Aside(recapAdvice(what))
}

// recapAdvice is the one wording, so every trigger says it the same way: the
// reason, then the call. No `from`: the span defaults to this turn, which is
// the commonest call and the one that cannot be misquoted.
func recapAdvice(detail string) string {
	return fmt.Sprintf("Context note — %s. If you no longer need it verbatim, remove it: "+
		"recap({summary: \"…what happened and what is true now…\"}), keeping anything still "+
		"needed with keep:[\"<tool name or a phrase from the call>\"]. Nothing is lost; it moves "+
		"to a file and leaves your context.", detail)
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
		if err != nil || tc.Function.Name == "recap" {
			// recap bounds its own reads; clipping them again would clip the
			// thing the model just asked to see.
			return out, err
		}
		// The cap is UNIVERSAL (USER). It began exec-only, but nothing about
		// the harm is specific to exec: node_read on a large file, a search with
		// many hits, an eval returning a big structure all cost the same window.
		// Structured results are clipped too, and knowingly — a clipped JSON is
		// still readable prose to a model, while an unbounded one only has to
		// start with "{" to reopen the hole.
		out = h.capOnce(tc.Function.Name+" "+oneLine(tc.Function.Arguments, 120), out)
		h.watchRecap(tc.Function.Name, len(out))
		return out, err
	}
}

// capOnce is the cap, memoized by content.
//
// It has to be applied in TWO places — once for the model (withRecapWatch) and
// once for the UI (cappedReporter) — because each tool reports its own call
// from inside its own wrapper, before the outer cap runs. Without memoizing,
// the same output would spill twice, under two refs, and the human would be
// told to read a copy the model has never heard of.
func (h *Harness) capOnce(command, out string) string {
	if len(out) <= execInlineMax {
		return out
	}
	sum := sha256.Sum256([]byte(out))
	key := hex.EncodeToString(sum[:])

	h.recapMu.Lock()
	if h.capped == nil {
		h.capped = map[string]string{}
	}
	if got, ok := h.capped[key]; ok {
		h.recapMu.Unlock()
		return got
	}
	h.recapMu.Unlock()

	capped := capExecOutput(out, command, h.spillExec)

	h.recapMu.Lock()
	if h.capped == nil {
		h.capped = map[string]string{}
	}
	h.capped[key] = capped
	h.recapMu.Unlock()
	return capped
}

// cappedReporter wraps the UI callback so what the human sees is what the MODEL
// saw.
//
// The -p stream used to carry the RAW result while the model got the clipped
// one: the TUI rendered a quarter-megabyte the model had never read, and anyone
// asking "why did it not see the last line?" was misled by their own screen.
// The harness has to say what actually happened.
func (h *Harness) cappedReporter(onCall func(string, map[string]any, string)) func(string, map[string]any, string) {
	if onCall == nil {
		return nil
	}
	return func(tool string, args map[string]any, result string) {
		h.workTool(workToolText(tool, args), result)
		onCall(tool, args, h.capOnce(tool+" "+oneLine(fmt.Sprint(args), 120), result))
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
