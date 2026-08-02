package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// ── reading back what was clipped ───────────────────────────────────
//
// The cap spills the whole result to a file. The first version told the model
// to "grep it with exec", which is wrong twice over: under --docker the file is
// on the HOST and the container has only the worktree mounted, so the path does
// not exist for the model at all; and with no exec backend there is no shell to
// grep with. Background job logs have carried the same broken instruction since
// they were written.
//
// So retrieval is dun-side and addressed by a REF — a short name printed in the
// gap — rather than by a path the model may have no way to open. Reads are
// bounded by the same cap that created the spill, or a "full" read would put
// back exactly what was taken out.

// grepWholeLine is the longest line returned in full by a grep read. Beyond it,
// only a window around each match comes back — the line itself is the problem.
const grepWholeLine = 2000

// grepContext is how much of a long line to show either side of a match, and
// grepMaxWindows caps how many matches answer at once.
const (
	grepContext    = 300
	grepMaxWindows = 20
)

// unlined reports whether the line-based readers are useless here: not "how
// many lines" but "is any single line bigger than a whole reply". A file of
// three lines where one is 96,000 characters is unlined for every practical
// purpose, and counting lines would call it structured. The saved file also
// carries a "$ command" header, so a one-line blob is never a one-line file.
func unlined(lines []string) bool {
	for _, l := range lines {
		if len(l) > execInlineMax {
			return true
		}
	}
	return false
}

// boundRead is the cap applied to recap's OWN answers.
//
// Live: asked for tail:100 of a three-line file, this handed the model the
// entire 98,000-character blob — the cap defeated through recap's own door, by
// the one tool that exists to manage the window. Every read is bounded, and a
// read that had to be clipped says how to get the rest properly rather than
// silently returning two ends again.
func boundRead(ref, out string) string {
	if len(out) <= execInlineMax {
		return out
	}
	return fmt.Sprintf("%s\n[that read was %d characters, too big to return whole — page it instead: "+
		"recap({ref:%q, at:0}), then the offset each page reports]", capExecOutput(out, "", nil), len(out), ref)
}

// pageOf returns one window of a result that cannot be read any other way, and
// says where the next one starts. Paging is the only handle on output with no
// lines to grep, and the reply has to carry the offset or the model must guess.
func pageOf(ref, body string, at int) string {
	if at < 0 {
		at = 0
	}
	if at >= len(body) {
		return fmt.Sprintf("%s: offset %d is past the end (%d characters).", ref, at, len(body))
	}
	end := at + execInlineMax
	if end > len(body) {
		end = len(body)
	}
	page := safeCutFront(safeCut(body[at:end]))
	more := ""
	if end < len(body) {
		more = fmt.Sprintf(" — next page: recap({ref:%q, at:%d})", ref, end)
	}
	return fmt.Sprintf("%s: characters %d–%d of %d%s\n%s", ref, at, end, len(body), more, page)
}

// registerSpill files a path under a short ref and returns it.
func (h *Harness) registerSpill(kind, path string) string {
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	if h.spills == nil {
		h.spills = map[string]string{}
	}
	h.spillSeq++
	ref := fmt.Sprintf("%s%d", kind, h.spillSeq)
	h.spills[ref] = path
	return ref
}

func (h *Harness) spillPath(ref string) string {
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	return h.spills[strings.TrimSpace(ref)]
}

// readSpill answers recap({ref: …}): the clipped result, or a job log, read
// without a shell and without leaving the process.
func (h *Harness) readSpill(ref, grep string, head, tail, at int, full bool) string {
	path := h.spillPath(ref)
	if path == "" {
		var known []string
		h.recapMu.Lock()
		for k := range h.spills {
			known = append(known, k)
		}
		h.recapMu.Unlock()
		sort.Strings(known)
		if len(known) == 0 {
			return "ERROR: no saved results yet. A ref appears in the gap when a result is too big to show whole."
		}
		return fmt.Sprintf("ERROR: no saved result %q. Known refs: %s", ref, strings.Join(known, ", "))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	body := string(data)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	// Output with no line structure — a minified JSON document, a base64 blob,
	// one enormous generated line — cannot be read with grep, head or tail:
	// every one of them is line-based, so they all hand back the single line
	// that was the problem. Such a result is only reachable by PAGING, so `at`
	// takes a character offset and the reply says where the next page starts.
	if at > 0 || (full && unlined(lines)) {
		return pageOf(ref, body, at)
	}

	switch {
	case grep != "":
		re, err := regexp.Compile(grep)
		if err != nil {
			return fmt.Sprintf("ERROR: %q is not a valid regexp: %v", grep, err)
		}
		// A match inside a very long line must come back as a WINDOW around the
		// match, not as the line. Live: grep for SECRET on a 98,000-character
		// single line returned that whole line, the clip took the middle, and
		// the middle was where the match was — so the one read that should have
		// answered the question threw the answer away. The model then spent six
		// calls hunting the spill file through the filesystem, which only worked
		// because exec had a host to search; under --docker it would have failed.
		var hit []string
		for i, l := range lines {
			if len(l) <= grepWholeLine {
				if re.MatchString(l) {
					hit = append(hit, fmt.Sprintf("%d: %s", i+1, l))
				}
				continue
			}
			for _, loc := range re.FindAllStringIndex(l, grepMaxWindows) {
				from, to := loc[0]-grepContext, loc[1]+grepContext
				if from < 0 {
					from = 0
				}
				if to > len(l) {
					to = len(l)
				}
				hit = append(hit, fmt.Sprintf("%d@%d: …%s…", i+1, loc[0], safeCutFront(safeCut(l[from:to]))))
			}
		}
		if len(hit) == 0 {
			return fmt.Sprintf("%s: no line matches %q (%d lines searched).", ref, grep, len(lines))
		}
		out := fmt.Sprintf("%s: %d of %d lines match %q\n%s", ref, len(hit), len(lines), grep, strings.Join(hit, "\n"))
		return capExecOutput(out, "", nil) // a match list can itself be enormous
	case head > 0:
		if head > len(lines) {
			head = len(lines)
		}
		return boundRead(ref, fmt.Sprintf("%s: first %d of %d lines\n%s", ref, head, len(lines),
			strings.Join(lines[:head], "\n")))
	case tail > 0:
		if tail > len(lines) {
			tail = len(lines)
		}
		return boundRead(ref, fmt.Sprintf("%s: last %d of %d lines\n%s", ref, tail, len(lines),
			strings.Join(lines[len(lines)-tail:], "\n")))
	case full:
		// Bounded on purpose. An unbounded "full" would hand back exactly what
		// the cap removed, which is the whole problem returning through a door
		// marked convenience.
		if len(body) <= execInlineMax {
			return fmt.Sprintf("%s: %d lines\n%s", ref, len(lines), body)
		}
		return fmt.Sprintf("%s: %d lines, %d characters — too large to show whole, so this is still clipped. "+
			"Narrow it with grep, head or tail, or page it with at:0, at:%d, …\n%s",
			ref, len(lines), len(body), execInlineMax, capExecOutput(body, "", nil))
	}
	shape := fmt.Sprintf("%d lines", len(lines))
	if unlined(lines) {
		shape += " (no line structure — grep/head/tail cannot help; page it with at:0)"
	}
	return fmt.Sprintf("%s: %s, %d characters. Ask for part of it: grep, head, tail, at:<offset>, or "+
		"full:true (still bounded).", ref, shape, len(body))
}

// ── the tool ────────────────────────────────────────────────────────

func recapToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "recap"
	td.Function.Description = "Manage your own context. TWO modes. (1) READ BACK a result that was too big to " +
		"show whole: recap({ref:\"r3\", grep:\"ERROR\"}) — also head, tail, or full (still bounded). The ref " +
		"is printed in the gap where the output was clipped, and works for background job logs too; it is read " +
		"directly, so it works with no shell and inside a container. (2) REPLACE a stretch of this conversation " +
		"with a corrected account of it. Use it after " +
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
			"ref":     map[string]any{"type": "string", "description": "read mode: a saved result's ref, as printed where its output was clipped"},
			"grep":    map[string]any{"type": "string", "description": "read mode: only lines matching this regexp"},
			"head":    map[string]any{"type": "integer", "description": "read mode: the first N lines"},
			"tail":    map[string]any{"type": "integer", "description": "read mode: the last N lines"},
			"full":    map[string]any{"type": "boolean", "description": "read mode: as much as the cap allows"},
			"from":    map[string]any{"type": "string", "description": "exact phrase from the message the churn began after"},
			"summary": map[string]any{"type": "string", "description": "what actually happened and what is true now, written to stand alone"},
			"keep": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "which tool results to keep verbatim: a tool NAME (\"exec\") or a phrase from its arguments (\"grep -c\"). Everything else in the span goes.",
			},
			"user": map[string]any{"type": "string", "description": "optional: a clearer wording of the anchoring user message"},
		},
	}
	return td
}

func withRecap(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "recap" {
			return inner(ctx, tc)
		}
		var args struct {
			Ref     string   `json:"ref"`
			Grep    string   `json:"grep"`
			Head    int      `json:"head"`
			Tail    int      `json:"tail"`
			At      int      `json:"at"`
			Full    bool     `json:"full"`
			From    string   `json:"from"`
			Summary string   `json:"summary"`
			Keep    []string `json:"keep"`
			User    string   `json:"user"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		// A ref means READ. The two modes share a tool because they are the same
		// job — deciding what is in your context — but they are told apart by
		// what was asked for, never by a mode flag the model has to remember.
		var out string
		if strings.TrimSpace(args.Ref) != "" {
			out = h.readSpill(args.Ref, args.Grep, args.Head, args.Tail, args.At, args.Full)
		} else {
			out = h.runRecap(ctx, args.From, args.Summary, args.Keep, args.User)
		}
		if onCall != nil {
			// Report what was actually ASKED. Read-mode calls used to display as
			// from:"" summary:"" — an empty rewrite — because only the rewrite
			// fields were passed on, so the UI showed a call nobody made.
			a := map[string]any{}
			if strings.TrimSpace(args.Ref) != "" {
				a["ref"] = args.Ref
				for k, v := range map[string]any{"grep": args.Grep, "head": args.Head, "tail": args.Tail, "at": args.At, "full": args.Full} {
					switch t := v.(type) {
					case string:
						if t != "" {
							a[k] = t
						}
					case int:
						if t != 0 {
							a[k] = t
						}
					case bool:
						if t {
							a[k] = t
						}
					}
				}
			} else {
				a["from"], a["summary"], a["keep"] = args.From, args.Summary, args.Keep
			}
			onCall("recap", a, out)
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

// spillRef saves an oversized result and returns the ref the model will use to
// read it back.
func (h *Harness) spillRef(command, output string) string {
	path := h.spillExec(command, output)
	if path == "" {
		return ""
	}
	return h.registerSpill("r", path)
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

	capped := capExecOutput(out, command, func(cmd, body string) string { return h.spillRef(cmd, body) })

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
