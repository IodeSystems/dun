package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
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
// Lesson: after the turn (learnAfterTurn), one side call per episode (kind
// "learn": no tools, no history, at most learnMaxTokens out, a prompt of at
// most churnKeep failed calls and the fix, each clipped) turns it into a
// candidate {name, detail, tags}. At most learnCap calls per session; an
// episode past the cap is dropped, not queued. The model may answer skip.
// The task's text is not in the prompt.
//
// Genericity (genericityError): deterministic heuristics reject a detail
// with a path, a file name, a URL, a hash or long number, quoted code, an
// identifier from the workspace or the episode, or the task's words. A
// lesson is for a different task: what is useless without this one is
// dropped, so a benchmark score measures retrieval, not memorization.
//
// The candidate is a strategy in the project's scope tagged candidate,
// written by strategyStore().apply as the memory tool's add (file, dund
// ingest), with a "proposed" entry in its attestation log. It is not
// important: only the user marks that. Recall leaves candidates out
// (memFinder: a tag filter in dun, as raglit's filter keeps tags and cannot
// exclude one). The user reviews them: /memories candidates, accept (the
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
	// learnTimeout bounds one lesson call.
	learnTimeout = 90 * time.Second

	// candidateTag marks a strategy the learning phase proposed and the
	// user has not accepted.
	candidateTag = "candidate"

	// learnDetailMax and learnSentencesMax bound a lesson's detail.
	learnDetailMax    = 400
	learnSentencesMax = 3
	// lessonOverlap is the share of the shorter text's words two lessons
	// must share to be the same lesson.
	lessonOverlap = 0.7
)

// churnAttempt is one tool call in brief.
type churnAttempt struct {
	Tool, Target, Args, Out string
}

// churnEpisode is a resolved streak: its length, its last failed calls
// (oldest first), and the call that worked.
type churnEpisode struct {
	Fails int
	Tried []churnAttempt
	Fix   churnAttempt
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
	calls   int        // lesson calls this session (learnCap)
	run     sync.Mutex // one flush at a time
}

// learnOn reports whether this session learns: the main agent, with a
// strategies store.
func (h *Harness) learnOn() bool { return !h.isChild() && h.strategyStore() != nil }

// learnQueue keeps a resolved episode for the end of the turn, while the
// session's cap has room.
func (h *Harness) learnQueue(ep *churnEpisode) {
	if ep == nil || !h.learnOn() {
		return
	}
	h.learn.mu.Lock()
	defer h.learn.mu.Unlock()
	if h.learn.calls+len(h.learn.pending) < learnCap {
		h.learn.pending = append(h.learn.pending, *ep)
	}
}

// learnAfterTurn turns the turn's episodes into candidates, in the
// background: the turn does not wait on it.
func (h *Harness) learnAfterTurn(ctx context.Context) {
	h.learn.mu.Lock()
	n := len(h.learn.pending)
	h.learn.mu.Unlock()
	if n == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), learnTimeout*learnCap)
	go func() {
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

// lesson is what the model proposes.
type lesson struct {
	Name   string   `json:"name"`
	Detail string   `json:"detail"`
	Tags   []string `json:"tags"`
	Skip   bool     `json:"skip"`
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
	c, err := parseLesson(reply)
	if err != nil {
		return "no lesson: " + err.Error()
	}
	if c.Skip {
		return "no lesson: the model found none that holds beyond this task"
	}
	c.Name = itemSlug(c.Name)
	if err := genericityError(c, h.learnTaskText(), episodeText(ep), workspaceNames(h.cfg.Workspace)); err != nil {
		return "rejected " + c.Name + ": " + err.Error()
	}
	if dup := h.lessonDuplicate(ctx, s, c); dup != "" {
		return "skipped " + c.Name + ": " + dup
	}
	out, err := s.apply(memoryArgs{Action: "add", Kind: kindStrategy, Name: c.Name, Content: c.Detail,
		Tags: append(slices.Clone(c.Tags), candidateTag)}, h.noteMeta())
	if err != nil {
		return "not saved " + c.Name + ": " + err.Error()
	}
	if p := s.named(kindStrategy, c.Name); p != "" {
		n, _ := readNote(p)
		_ = s.attest(p, attestEntry{Kind: "proposed", Unit: n.unit(), By: "dun:learn",
			Note: fmt.Sprintf("learning phase: %d failed calls in a row, then a %s call worked", ep.Fails, ep.Fix.Tool)})
	}
	if h.cfg.OnNotify != nil {
		h.cfg.OnNotify("learned a candidate strategy:" + c.Name + " — /memories candidates to accept or reject it")
	}
	return out
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

// learnPrompt is the episode in brief, after learnInstruction.
func learnPrompt(ep churnEpisode) string {
	var b strings.Builder
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
	ch, err := untee(h.Session.Runner).ChatStream(ctx, []llm.Message{{Role: "user", Content: prompt}}, nil, &llm.ChatOpts{MaxTokens: learnMaxTokens})
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

// parseLesson reads the model's JSON object, fenced or not.
func parseLesson(reply string) (lesson, error) {
	var c lesson
	i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if i < 0 || j < i {
		return c, fmt.Errorf("the answer holds no JSON object")
	}
	if err := json.Unmarshal([]byte(reply[i:j+1]), &c); err != nil {
		return c, fmt.Errorf("the answer is not the JSON asked for: %v", err)
	}
	c.Detail = flatLine(c.Detail)
	if !c.Skip && (itemSlug(c.Name) == "" || c.Detail == "") {
		return c, fmt.Errorf("the answer has no name or no detail")
	}
	return c, nil
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

// workspaceNamesMax bounds the walk for workspaceNames.
const workspaceNamesMax = 5000

// workspaceNames is the names of the workspace's files and directories,
// three levels down, lower case: each name and its stem (before the first
// dot). Dot directories, node_modules and vendor are skipped.
func workspaceNames(root string) map[string]bool {
	out := map[string]bool{}
	if root == "" {
		return out
	}
	root = filepath.Clean(root)
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		if n++; n > workspaceNamesMax {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" ||
				strings.Count(strings.TrimPrefix(p, root), string(os.PathSeparator)) >= 3 {
				return filepath.SkipDir
			}
		}
		l := strings.ToLower(name)
		out[l] = true
		if stem, _, ok := strings.Cut(l, "."); ok && stem != "" {
			out[stem] = true
		}
		return nil
	})
	return out
}

var (
	// A path: /a, ./a, ../a, ~/a, a/b/c, C:\a.
	lessonPathRe = regexp.MustCompile(`(^|[\s"'(\x60=:])(~|\.{1,2})?/[\w.-]+|\w+/[\w.-]+/[\w.-]+|\b[A-Za-z]:\\`)
	// A file name with a common extension.
	lessonFileRe = regexp.MustCompile(`(?i)\b[\w-]+\.(py|go|js|mjs|ts|tsx|jsx|c|h|cc|cpp|hpp|rs|java|kt|rb|php|cs|swift|json|ya?ml|toml|ini|cfg|conf|txt|md|rst|sh|bash|csv|tsv|html?|css|sql|log|xml|lock|mod|sum|db|sqlite|so|tar|gz|tgz|zip|whl|jar|pdf|png|jpe?g|svg|wasm|bin|out|exe|dll)\b`)
	lessonURLRe  = regexp.MustCompile(`(?i)[a-z]+://|\bwww\.`)
	// A hash, a long number, an IP address.
	lessonHashRe = regexp.MustCompile(`(?i)\b[0-9a-f]{7,}\b`)
	lessonNumRe  = regexp.MustCompile(`\b\d{5,}\b|\b\d{1,3}(\.\d{1,3}){3}\b`)
	// A quoted span: backticks or double quotes.
	lessonQuoteRe = regexp.MustCompile("`[^`]*`|\"[^\"]*\"")
	// A token: letters, digits and _, with . - inside.
	lessonTokenRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_.\-]*[A-Za-z0-9_]|[A-Za-z0-9_]`)
	lessonSentRe  = regexp.MustCompile(`[.!?](\s|$)`)
)

// lessonQuoteMax is the longest quoted span a lesson may hold: a command
// name, not a line of code or output.
const lessonQuoteMax = 40

// genericityError says why a lesson is about its task, nil when it is not.
// Heuristics, in order:
//   - shape: detail at most learnDetailMax bytes and learnSentencesMax
//     sentences; a name of at most 6 words;
//   - always rejected: a path, a file name with a common extension, a URL,
//     a hash (7+ hex characters with a digit and a letter), a number of 5+
//     digits or an IP address, a code fence, a quoted span longer than
//     lessonQuoteMax;
//   - rejected when the task or the workspace has it: a workspace file or
//     directory name that looks like an identifier (identLike); a token
//     the task marks (marked: quoted, or with a digit, _, ., or an inner
//     capital); five words in a row of the task's text;
//   - rejected when the episode has it: an identifier (snake_case, an
//     inner capital, dotted.name; not an ALL_CAPS variable).
func genericityError(c lesson, task, episode string, names map[string]bool) error {
	text := c.Name + " " + c.Detail + " " + strings.Join(c.Tags, " ")
	switch {
	case len(c.Detail) > learnDetailMax:
		return fmt.Errorf("the detail is %d bytes; at most %d", len(c.Detail), learnDetailMax)
	case len(lessonSentRe.FindAllString(c.Detail+" ", -1)) > learnSentencesMax:
		return fmt.Errorf("the detail is more than %d sentences", learnSentencesMax)
	case len(strings.Split(c.Name, "-")) > 6:
		return fmt.Errorf("the name is more than 6 words")
	case strings.Contains(text, "```"):
		return fmt.Errorf("it quotes code")
	}
	for _, q := range lessonQuoteRe.FindAllString(c.Detail, -1) {
		if len(q) > lessonQuoteMax+2 {
			return fmt.Errorf("it quotes %d characters", len(q)-2)
		}
	}
	for _, r := range []struct {
		re   *regexp.Regexp
		what string
	}{{lessonURLRe, "a URL"}, {lessonPathRe, "a path"}, {lessonFileRe, "a file name"}, {lessonHashRe, "a hash"}, {lessonNumRe, "a number or address"}} {
		for _, m := range r.re.FindAllString(text, -1) {
			// A hash has a digit and a letter: "defaced" is a word.
			if r.re == lessonHashRe && (!strings.ContainsAny(m, "0123456789") || strings.Trim(m, "0123456789") == "") {
				continue
			}
			return fmt.Errorf("it holds %s (%s)", r.what, strings.TrimSpace(m))
		}
	}
	marked := taskMarked(task)
	epIdents := map[string]bool{}
	for _, t := range lessonTokenRe.FindAllString(episode, -1) {
		if identLike(t, false) {
			epIdents[strings.ToLower(t)] = true
		}
	}
	for _, t := range lessonTokenRe.FindAllString(text, -1) {
		l := strings.ToLower(t)
		switch {
		case names[l] && (identLike(t, true) || identLike(l, true)):
			return fmt.Errorf("it names %q from the workspace", t)
		case marked[l]:
			return fmt.Errorf("it names %q from the task", t)
		case epIdents[l]:
			return fmt.Errorf("it names %q from the failed calls", t)
		}
	}
	if s := sharedRun(c.Detail, task, 5); s != "" {
		return fmt.Errorf("it repeats the task (%q)", s)
	}
	return nil
}

// identLike reports whether t looks like a name in code: it has _ (not an
// ALL_CAPS variable), a dot between letters, or a capital after a lower
// case letter; with digits, also a letter and a digit together or a - .
func identLike(t string, digits bool) bool {
	hasLower, hasUpper, hasDigit, hasLetter := false, false, false, false
	inner := false
	for i, r := range t {
		switch {
		case unicode.IsLower(r):
			hasLower, hasLetter = true, true
		case unicode.IsUpper(r):
			hasUpper, hasLetter = true, true
			if i > 0 && hasLower {
				inner = true
			}
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	switch {
	case strings.Contains(t, "_") && hasLower:
		return true
	case strings.Contains(strings.Trim(t, "."), ".") && hasLetter:
		return true
	case inner:
		return true
	case digits && hasDigit && hasLetter:
		return true
	case digits && strings.Contains(t, "-") && hasUpper:
		return true
	}
	return false
}

// taskMarked is the tokens of the task that name something of its own,
// lower case: each token of a quoted span, and each token identLike with
// digits.
func taskMarked(task string) map[string]bool {
	out := map[string]bool{}
	for _, q := range lessonQuoteRe.FindAllString(task, -1) {
		for _, t := range lessonTokenRe.FindAllString(q, -1) {
			if len(t) >= 3 && !stopWords[strings.ToLower(t)] {
				out[strings.ToLower(t)] = true
			}
		}
	}
	for _, t := range lessonTokenRe.FindAllString(task, -1) {
		if identLike(t, true) {
			out[strings.ToLower(t)] = true
		}
	}
	return out
}

// sharedRun is the first run of n words a and b share, "" with none.
func sharedRun(a, b string, n int) string {
	bw := wordsOf(b)
	if len(bw) < n {
		return ""
	}
	runs := map[string]bool{}
	for i := 0; i+n <= len(bw); i++ {
		runs[strings.Join(bw[i:i+n], " ")] = true
	}
	aw := wordsOf(a)
	for i := 0; i+n <= len(aw); i++ {
		if s := strings.Join(aw[i:i+n], " "); runs[s] {
			return s
		}
	}
	return ""
}

// lessonDuplicate names the strategy c repeats, "" when it is new: one of
// its name (current or voided: a rejected lesson is not proposed again),
// or one that shares lessonOverlap of the shorter text's words, found by
// the strategies index (dund down: the files) or among the voided ones.
func (h *Harness) lessonDuplicate(ctx context.Context, s *memoryStore, c lesson) string {
	if p := s.named(kindStrategy, c.Name); p != "" {
		return "strategy:" + c.Name + " exists"
	}
	terms := queryTerms(c.Name + " " + c.Detail)
	same := func(p string) bool {
		n, err := readNote(p)
		return err == nil && noteKind(n.meta) == kindStrategy &&
			termOverlap(terms, queryTerms(noteNameOf(strings.TrimSuffix(p, voidSuffix), n.meta)+" "+n.body)) >= lessonOverlap
	}
	for _, d := range s.dirs() {
		voided, _ := filepath.Glob(filepath.Join(d, "*.md"+voidSuffix))
		for _, p := range voided {
			n, err := readNote(p)
			if err == nil && noteKind(n.meta) == kindStrategy && noteNameOf(strings.TrimSuffix(p, voidSuffix), n.meta) == c.Name {
				return "strategy:" + c.Name + " was voided"
			}
			if same(p) {
				return "like the voided " + filepath.Base(p)
			}
		}
	}
	if l := h.stratLink(); l != nil {
		hits, _ := (&memFinder{l: l, m: s, candidates: true}).Find(ctx, []string{c.Name + " " + c.Detail})
		for _, hit := range hits {
			if same(hit.DocID) {
				n, _ := readNote(hit.DocID)
				return "like strategy:" + noteNameOf(hit.DocID, n.meta)
			}
		}
	}
	return ""
}

// termOverlap is the share of the shorter list's terms the other holds.
func termOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(b) < len(a) {
		a, b = b, a
	}
	in := map[string]bool{}
	for _, t := range b {
		in[t] = true
	}
	n := 0
	for _, t := range a {
		if in[t] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// isCandidate reports whether the item at path is a candidate.
func isCandidate(path string) bool {
	n, err := readNote(path)
	return err == nil && slices.Contains(noteTags(n.meta), candidateTag)
}

// candidatesListing is /memories candidates.
func (h *Harness) candidatesListing() (string, error) {
	s := h.strategyStore()
	if s == nil {
		return "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
	}
	var b strings.Builder
	n := 0
	for _, i := range s.list() {
		if i.Void || !slices.Contains(i.Tags, candidateTag) {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n  %s:%s  %s  · %s", i.Kind, i.Name, i.Body, i.Date)
	}
	if n == 0 {
		return "candidates: none — the learning phase proposes a strategy when a run of failed calls ends in one that works", nil
	}
	return fmt.Sprintf("candidates — %d proposed strategies, not used until accepted:", n) + b.String() +
		"\n/memories accept <kind:name> to use one, /memories reject <kind:name> to void it", nil
}

// reviewCandidate is /memories accept and reject.
func (h *Harness) reviewCandidate(verb, arg string) (string, error) {
	m, path, err := h.itemArg(arg)
	if err != nil {
		return "", err
	}
	if !isCandidate(path) {
		return "", fmt.Errorf("%s is not a candidate", arg)
	}
	if verb == "reject" {
		return m.apply(memoryArgs{Action: "void", OldText: arg, Why: "rejected by the user with /memories reject", Source: "user"}, h.noteMeta())
	}
	n, err := readNote(path)
	if err != nil {
		return "", err
	}
	if err := m.attest(path, attestEntry{Kind: "confirmed", Unit: n.unit(), Note: "accepted by the user with /memories accept", By: "user"}); err != nil {
		return "", err
	}
	n.meta["confirmed"] = m.now().Format("2006-01-02")
	tags := slices.DeleteFunc(noteTags(n.meta), func(t string) bool { return t == candidateTag })
	down, err := m.write(path, memoryArgs{Content: n.body, Question: n.meta["question"], Public: n.meta["public"] == "true", Tags: tags},
		noteMeta{extra: n.meta, tagOnly: true})
	if err != nil {
		return "", err
	}
	return arg + ": accepted, found by search from now on" + down, nil
}
