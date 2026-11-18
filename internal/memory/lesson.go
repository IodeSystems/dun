package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/iodesystems/dun/internal/textfmt"
)

// A lesson: what the learning phase (harness learn.go, docs/strategies.md
// "Learning phase") proposes from a resolved episode, checked here and
// kept as a candidate strategy, tagged candidate until the user accepts it.
// The episodes, the gate and the lesson call are the harness's; the check
// (genericityError), the duplicate test (lessonDuplicate), the write
// (Propose) and the user's review (/memories candidates, accept, reject)
// are the store's.

const (
	// CandidateTag marks a strategy the learning phase proposed and the
	// user has not accepted.
	CandidateTag = "candidate"

	// learnDetailMax and learnSentencesMax bound a lesson's detail.
	learnDetailMax    = 400
	learnSentencesMax = 3
	// lessonOverlap is the share of the shorter text's words two lessons
	// must share to be the same lesson.
	lessonOverlap = 0.7
)

// lesson is what the model proposes.
type lesson struct {
	Name   string   `json:"name"`
	Detail string   `json:"detail"`
	Tags   []string `json:"tags"`
	Skip   bool     `json:"skip"`
}

// LessonSource is what a lesson is checked against and written with.
type LessonSource struct {
	// Task is the session's user messages (the task, for the filter);
	// Calls everything the episode's calls said; Workspace the root whose
	// file names a lesson may not hold.
	Task      func() string
	Calls     string
	Workspace string
	// Note is the candidate's proposed entry: where it came from. Meta is
	// where the write says it came from.
	Note string
	Meta func() NoteMeta
}

// Propose reads the model's reply as a lesson, checks it and writes it to
// the strategies store s as a candidate, and says what came of it. name is
// the candidate's name when one was written, else "". l (may be nil) is the
// strategies' link, for the duplicate search.
func (s *Store) Propose(ctx context.Context, l *Link, reply string, src LessonSource) (out, name string) {
	c, err := parseLesson(reply)
	if err != nil {
		return "no lesson: " + err.Error(), ""
	}
	if c.Skip {
		return "no lesson: the model found none that holds beyond this task", ""
	}
	c.Name = itemSlug(c.Name)
	if err := genericityError(c, src.Task(), src.Calls, workspaceNames(src.Workspace)); err != nil {
		return "rejected " + c.Name + ": " + err.Error(), ""
	}
	if dup, cand := lessonDuplicate(ctx, l, s, c); dup != "" {
		if cand != "" {
			n, _ := readNote(cand)
			_ = s.attest(cand, AttestEntry{Kind: "recurred", Unit: n.unit(), By: "dun:learn", Note: "proposed again as " + c.Name})
			return "recurred " + c.Name + ": " + dup, ""
		}
		return "skipped " + c.Name + ": " + dup, ""
	}
	out, err = s.apply(toolArgs{Action: "add", Kind: KindStrategy, Name: c.Name, Content: c.Detail,
		Tags: append(slices.Clone(c.Tags), CandidateTag)}, src.Meta())
	if err != nil {
		return "not saved " + c.Name + ": " + err.Error(), ""
	}
	if p := s.Named(KindStrategy, c.Name); p != "" {
		n, _ := readNote(p)
		_ = s.attest(p, AttestEntry{Kind: "proposed", Unit: n.unit(), By: "dun:learn",
			Note: src.Note})
	}
	return out, c.Name
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
	c.Detail = textfmt.OneLine(c.Detail)
	if !c.Skip && (itemSlug(c.Name) == "" || c.Detail == "") {
		return c, fmt.Errorf("the answer has no name or no detail")
	}
	return c, nil
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
//
// cand is the current candidate it repeats, "" for any other match: a
// recurrence is counted on a candidate only.
func lessonDuplicate(ctx context.Context, l *Link, s *Store, c lesson) (dup, cand string) {
	ifCand := func(p string) string {
		if IsCandidate(p) {
			return p
		}
		return ""
	}
	if p := s.Named(KindStrategy, c.Name); p != "" {
		return "strategy:" + c.Name + " exists", ifCand(p)
	}
	terms := queryTerms(c.Name + " " + c.Detail)
	same := func(p string) bool {
		n, err := readNote(p)
		return err == nil && noteKind(n.meta) == KindStrategy &&
			termOverlap(terms, queryTerms(noteNameOf(strings.TrimSuffix(p, voidSuffix), n.meta)+" "+n.body)) >= lessonOverlap
	}
	for _, d := range s.dirs() {
		voided, _ := filepath.Glob(filepath.Join(d, "*.md"+voidSuffix))
		for _, p := range voided {
			n, err := readNote(p)
			if err == nil && noteKind(n.meta) == KindStrategy && noteNameOf(strings.TrimSuffix(p, voidSuffix), n.meta) == c.Name {
				return "strategy:" + c.Name + " was voided", ""
			}
			if same(p) {
				return "like the voided " + filepath.Base(p), ""
			}
		}
	}
	if l != nil {
		hits, _ := (&Finder{l: l, m: s, Candidates: true}).Find(ctx, []string{c.Name + " " + c.Detail})
		for _, hit := range hits {
			if same(hit.DocID) {
				n, _ := readNote(hit.DocID)
				return "like strategy:" + noteNameOf(hit.DocID, n.meta), ifCand(hit.DocID)
			}
		}
	}
	return "", ""
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

// IsCandidate reports whether the item at path is a candidate.
func IsCandidate(path string) bool {
	n, err := readNote(path)
	return err == nil && slices.Contains(noteTags(n.meta), CandidateTag)
}

// candidatesListing is /memories candidates.
func (st Stores) candidatesListing() (string, error) {
	s := st.Strategies
	if s == nil {
		return "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
	}
	var b strings.Builder
	n := 0
	for _, i := range s.List() {
		if i.Void || !slices.Contains(i.Tags, CandidateTag) {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n  %s:%s  %s  · %s", i.Kind, i.Name, i.Body, i.Date)
		if i.Recurred > 0 {
			fmt.Fprintf(&b, " · seen %d×", i.Recurred+1)
		}
	}
	if n == 0 {
		return "candidates: none — the learning phase proposes a strategy when a run of failed calls ends in one that works", nil
	}
	return fmt.Sprintf("candidates — %d proposed strategies, not used until accepted:", n) + b.String() +
		"\n/memories accept <kind:name> to use one, /memories reject <kind:name> to void it", nil
}

// reviewCandidate is /memories accept and reject.
func (st Stores) reviewCandidate(verb, arg string, meta NoteMeta) (string, error) {
	m, path, err := st.itemArg(arg)
	if err != nil {
		return "", err
	}
	if !IsCandidate(path) {
		return "", fmt.Errorf("%s is not a candidate", arg)
	}
	if verb == "reject" {
		return m.apply(toolArgs{Action: "void", OldText: arg, Why: "rejected by the user with /memories reject", Source: "user"}, meta)
	}
	n, err := readNote(path)
	if err != nil {
		return "", err
	}
	if err := m.attest(path, AttestEntry{Kind: "confirmed", Unit: n.unit(), Note: "accepted by the user with /memories accept", By: "user"}); err != nil {
		return "", err
	}
	n.meta["confirmed"] = m.now().Format("2006-01-02")
	tags := slices.DeleteFunc(noteTags(n.meta), func(t string) bool { return t == CandidateTag })
	down, err := m.write(path, toolArgs{Content: n.body, Question: n.meta["question"], Public: n.meta["public"] == "true", Tags: tags},
		NoteMeta{extra: n.meta, tagOnly: true})
	if err != nil {
		return "", err
	}
	return arg + ": accepted, found by search from now on" + down, nil
}
