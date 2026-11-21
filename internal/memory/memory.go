package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunhome"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// Memory — notes a later session on this project needs and cannot get from the
// code, the git history or the docs (plan item 9).
//
// A note is one markdown file in ~/.dun/memory/<project>/ (scope project) or
// ~/.dun/memory/@global/ (scope global), put into dund's memories index of
// that project or of the global set (docs/rag.md, slice 5). That index is the
// reason a note is worth writing: the proactive search (docsPreparer, through
// recall.go) brings a note back into a later conversation unasked, inlined
// whole, when that conversation is about it (M0: right note on 10 of 11
// probes that share its words, 0 wrong). The tool is there whenever the
// session has a RagLink, with or without the docs server. With dund down,
// a note is still written, the search falls back to the notes on disk, and
// the index catches up on the next contact (recall.go).
//
// Writes go straight to disk, with no approval step (USER, 2026-10-19), after
// a scan for text that would act on a later prompt instead of informing it.
// A note is found again by a phrase from it, not by an id: a model that has to
// carry ids between calls invents them (the exec_2 lesson in the icebox).
//
// With a registered remote project (/rag on), a note also goes to the
// raglit project's layers: private by default, public when its frontmatter
// says public: true (leak is irreversible, so the default stays private).
// A refine re-uploads the note under the same name to the same layer; a void
// forgets it from the layer and keeps the file. A layer failure is a
// down-note, never a hard failure: the file is the record (recall.go).

// NotesIndex is dund's index notes go into, a project's or the global
// set's (internal/ragd).
const NotesIndex = "memories"

// globalMemoryDir is the directory of the notes of scope global, under
// memory/. '@' is in no project slug (normalizeProject), so no project's
// notes share it.
const globalMemoryDir = "@global"

// memoryMaxTags and memoryTagMax cap a note's tags: a few short words.
const (
	memoryMaxTags = 8
	memoryTagMax  = 40
)

// memoryMaxChars caps a note's text, so the whole file (with frontmatter)
// stays under inlineMaxChars and surfaces inline instead of as a pointer.
const memoryMaxChars = 1200

// memoryDir is where a project's notes live. $DUN_HOME overrides ~/.dun, as
// for sessions.
func memoryDir(project string) string {
	return filepath.Join(dunhome.Dir(), "memory", project)
}

func ToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "memory"
	td.Function.Description = "Save a note for later sessions on this project, or fix one. A saved note comes back " +
		"to you unasked when a later conversation is about it. Save ONLY what a later session would need and could " +
		"not get from the code, the git history or the docs: the user's preferences and corrections, decisions and " +
		"their reasons, pitfalls found the hard way. Do NOT save code facts, file contents or task progress. One " +
		"fact per note, written to stand alone — the reader will not see this conversation. action add writes a " +
		"new note. A note records what was true when it was written: when the user says otherwise or the code " +
		"shows otherwise, refine it (new content) or void it (no longer applies), with why and source; confirm " +
		"one you checked against the code and found right. refine, void and confirm find the note by old_text, a " +
		"phrase copied from it or its question, or its path (memory:<name>). kind strategy saves an approach that " +
		"worked and will work again (name: a short slug; content: the detail, optional); kind constraint saves a " +
		"rule the user set for how to work, only when the user states it. Read any by its path with file_read: " +
		"memory:<name>, strategy:<name>, constraint:<name>."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"add", "refine", "void", "confirm"}},
			"kind": map[string]any{"type": "string", "enum": []string{KindMemory, KindStrategy, KindConstraint},
				"description": "memory (default): a fact; strategy: an approach; constraint: a rule the user set"},
			"name":     map[string]any{"type": "string", "description": "add: a short name, a few words (made a slug); required for strategy and constraint"},
			"content":  map[string]any{"type": "string", "description": "add/refine: the note — the fact and where it applies, and why it holds only if that was said; never add a reason or consequence nobody stated, and no dates (dun records when)"},
			"question": map[string]any{"type": "string", "description": "add/refine: the question this note answers, in the words a later user would ask it"},
			"old_text": map[string]any{"type": "string", "description": "refine/void/confirm: a phrase copied exactly from the note or its question, or its path (strategy:<name>)"},
			"why":      map[string]any{"type": "string", "description": "refine/void/confirm: the evidence — the user's own words, or the file you read and what it says. A confirm needs that check done first: \"the user asked me to check\" is not evidence. No dates — dun records when"},
			"source":   map[string]any{"type": "string", "enum": []string{"user", "code"}, "description": "refine/void/confirm: who says so — the user, or the code as it is now"},
			"public":   map[string]any{"type": "boolean", "description": "add: share with everyone on this project (default: private to you)"},
			"scope": map[string]any{"type": "string", "enum": []string{"project", "global"},
				"description": "add: project (default) for a fact about this project; global for one that holds in every project, such as how the user works"},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "add/refine: a few short topic words, such as build or deploy (refine: replaces them)"},
		},
		"required": []string{"action"},
	}
	return td
}

type toolArgs struct {
	Action   string   `json:"action"`
	Content  string   `json:"content"`
	Question string   `json:"question"`
	OldText  string   `json:"old_text"`
	Why      string   `json:"why"`
	Source   string   `json:"source"`
	Public   bool     `json:"public"`
	Scope    string   `json:"scope"`
	Tags     []string `json:"tags"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
}

// NoteMeta is what the frontmatter records about where a note came from.
type NoteMeta struct {
	Session string
	Commit  string
	// extra is a rewritten note's existing frontmatter, kept over the above.
	extra map[string]string
	// tagOnly: the rewrite leaves the note's text as it was (important,
	// unimportant, a refine of the tags alone), so the index gets its tags
	// (retag) and no ingest.
	tagOnly bool
}

// Store writes, replaces and removes notes in two directories (the
// project's and the global one), and keeps the index in step. ingest and
// forget are nil when there is no index; an error from either that wraps
// ErrDown is a note to the model, not a failure: the file is the record
// and the index catches up (recall.go).
type Store struct {
	dir    string
	global string // "" when there is no global scope
	// index is the dund index the files go into ("" is memories), kinds
	// the kinds kept here (nil is memory only), groups the surfacing groups
	// of a project and a global hit (zero: the memory groups). One shape
	// for every kind (kinds.go).
	index  string
	kinds  []string
	groups [2]string
	ingest func(path string) error
	retag  func(path string) error
	forget func(path string) error
	now    func() time.Time
}

// Dir is the directory of the project's notes, GlobalDir of the global
// ones ("" when the store keeps none).
func (m *Store) Dir() string       { return m.dir }
func (m *Store) GlobalDir() string { return m.global }

// dirs is the directories the store's notes are in.
func (m *Store) dirs() []string {
	if m.global == "" {
		return []string{m.dir}
	}
	return []string{m.dir, m.global}
}

// downNote is the line added to a call's result when the index could not be
// reached: said, so the model does not retry a write that happened.
const downNote = " — dund is not reachable: the note is on disk, and it is indexed when dund is"

// apply runs one memory call. The error is for the model to read: a bad call
// is answered, never fatal.
func (m *Store) apply(a toolArgs, meta NoteMeta) (string, error) {
	switch a.Action {
	case "add":
		kind, err := m.kindOf(a.Kind)
		if err != nil {
			return "", err
		}
		a.Kind = kind
		if err := scanItem(a); err != nil {
			return "", err
		}
		tags, err := cleanTags(a.Tags)
		if err != nil {
			return "", err
		}
		a.Tags = tags
		dir := m.dir
		switch a.Scope {
		case "", "project":
		case "global":
			if m.global == "" {
				return "", fmt.Errorf("this session keeps no global notes")
			}
			dir = m.global
		default:
			return "", fmt.Errorf("scope must be project or global, not %q", a.Scope)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		path, err := m.newPath(dir, &a)
		if err != nil {
			return "", err
		}
		down, err := m.write(path, a, meta)
		if err != nil {
			return "", err
		}
		out := "saved " + filepath.Base(path) + " (" + a.Kind + ":" + a.Name + ")"
		if dir == m.global && m.global != "" {
			out += " (global)"
		}
		return out + down, nil
	case "refine", "replace":
		path, err := m.find(a.OldText)
		if err != nil {
			return "", err
		}
		if err := scanNote(a.Content, a.Question); err != nil {
			return "", err
		}
		by, err := attestBy(a)
		if err != nil {
			return "", err
		}
		old, err := readNote(path)
		if err != nil {
			return "", err
		}
		if a.Question == "" {
			a.Question = old.meta["question"]
		}
		if a.Tags == nil {
			a.Tags = noteTags(old.meta)
		}
		if a.Tags, err = cleanTags(a.Tags); err != nil {
			return "", err
		}
		// A candidate stays one: only the user accepts it (lesson.go).
		if slices.Contains(noteTags(old.meta), CandidateTag) && !slices.Contains(a.Tags, CandidateTag) {
			a.Tags = append(a.Tags, CandidateTag)
		}
		a.Public = old.meta["public"] == "true"
		if err := m.attest(path, AttestEntry{Kind: "corrected", Unit: old.unit(), Text: strings.TrimSpace(a.Content),
			Note: a.Why, By: by, Was: old.body}); err != nil {
			return "", err
		}
		// Same file, so the index replaces the document instead of adding one.
		// The same text and question with other tags is a retag: no ingest.
		retag := strings.TrimSpace(a.Content) == strings.TrimSpace(old.body) && a.Question == old.meta["question"]
		old.meta["refined"] = m.now().Format("2006-01-02")
		delete(old.meta, "confirmed") // a confirmation was of the text this replaces
		down, err := m.write(path, a, NoteMeta{Session: meta.Session, Commit: meta.Commit, extra: old.meta, tagOnly: retag})
		if err != nil {
			return "", err
		}
		return "refined " + filepath.Base(path) + " (the old text is kept in its log)" + down, nil
	case "void", "remove":
		path, err := m.find(a.OldText)
		if err != nil {
			return "", err
		}
		by, err := attestBy(a)
		if err != nil {
			return "", err
		}
		old, err := readNote(path)
		if err != nil {
			return "", err
		}
		// Out of the index first: a log line for a void that then failed
		// would say the note is gone while search still serves it. With dund
		// down, the void goes ahead: the next contact removes every *.md.void
		// from the index (recall.go), and the search on disk reads *.md.
		down := ""
		if m.forget != nil {
			if err := m.forget(path); errors.Is(err, ErrDown) {
				down = " — dund is not reachable: it leaves the index when dund is"
			} else if err != nil {
				return "", fmt.Errorf("not voided: the index refused it: %w", err)
			}
		}
		// unsupported, not retract: in attest, retract withdraws an earlier
		// ruling and leaves the unit untouched; unsupported is the one
		// verdict that stops a claim counting.
		if err := m.attest(path, AttestEntry{Kind: "unsupported", Unit: old.unit(), Note: a.Why, By: by}); err != nil {
			return "", err
		}
		// Kept, out of the *.md set that is listed and indexed: nothing a
		// note said is destroyed.
		if err := os.Rename(path, path+voidSuffix); err != nil {
			return "", err
		}
		return "voided " + filepath.Base(path) + " (kept as " + filepath.Base(path+voidSuffix) + ")" + down, nil
	case "confirm":
		path, err := m.find(a.OldText)
		if err != nil {
			return "", err
		}
		by, err := attestBy(a)
		if err != nil {
			return "", err
		}
		old, err := readNote(path)
		if err != nil {
			return "", err
		}
		if err := m.attest(path, AttestEntry{Kind: "confirmed", Unit: old.unit(), Note: a.Why, By: by}); err != nil {
			return "", err
		}
		old.meta["confirmed"] = m.now().Format("2006-01-02")
		down, err := m.write(path, toolArgs{Content: old.body, Question: old.meta["question"], Public: old.meta["public"] == "true",
			Tags: noteTags(old.meta)}, NoteMeta{extra: old.meta})
		if err != nil {
			return "", err
		}
		return "confirmed " + filepath.Base(path) + down, nil
	default:
		return "", fmt.Errorf("action must be add, refine, void or confirm, not %q", a.Action)
	}
}

// write writes the note and indexes it. down is downNote when the index
// could not be reached, else "".
func (m *Store) write(path string, a toolArgs, meta NoteMeta) (down string, err error) {
	// A rewritten note keeps when and where it was first written; refined
	// and confirmed say what has happened to it since.
	field := func(k, v string) string {
		if old := meta.extra[k]; old != "" {
			return old
		}
		return v
	}
	var b strings.Builder
	b.WriteString("---\n")
	if v := field("name", a.Name); v != "" {
		b.WriteString("name: " + v + "\n")
	}
	if v := field("kind", a.Kind); v != "" && v != KindMemory {
		b.WriteString("kind: " + v + "\n")
	}
	if meta.extra["important"] == "true" {
		b.WriteString("important: true\n")
	}
	if q := strings.TrimSpace(a.Question); q != "" {
		b.WriteString("question: " + q + "\n")
	}
	b.WriteString("date: " + field("date", m.now().Format("2006-01-02")) + "\n")
	if v := field("session", meta.Session); v != "" {
		b.WriteString("session: " + v + "\n")
	}
	if v := field("commit", meta.Commit); v != "" {
		b.WriteString("commit: " + v + "\n")
	}
	for _, k := range []string{"refined", "confirmed"} {
		if v := meta.extra[k]; v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	if len(a.Tags) > 0 {
		b.WriteString("tags: " + strings.Join(a.Tags, ", ") + "\n")
	}
	fmt.Fprintf(&b, "public: %t\n", a.Public)
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(a.Content) + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	index := m.ingest
	if meta.tagOnly && m.retag != nil {
		index = m.retag
	}
	if index != nil {
		if err := index(path); errors.Is(err, ErrDown) {
			return downNote, nil
		} else if err != nil {
			// The file is the record; the index can be rebuilt from it. Say so
			// rather than fail the call, or the model retries a write that
			// already happened.
			log.Printf("dun: memory: %s saved but not indexed: %v", filepath.Base(path), err)
		}
	}
	return "", nil
}

// cleanTags trims, lower-cases and de-duplicates tags, and refuses what
// would not fit the frontmatter line they are written on.
func cleanTags(tags []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
		if t == "" || seen[t] {
			continue
		}
		if strings.ContainsAny(t, ",:") || len([]rune(t)) > memoryTagMax {
			return nil, fmt.Errorf("tag %q: a tag is a few words, at most %d characters, with no comma or colon", t, memoryTagMax)
		}
		if err := scanNote(t, ""); err != nil {
			return nil, fmt.Errorf("tag %q: %w", t, err)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > memoryMaxTags {
		return nil, fmt.Errorf("%d tags; at most %d", len(out), memoryMaxTags)
	}
	return out, nil
}

// noteTags is a note's tags, from its frontmatter.
func noteTags(meta map[string]string) []string {
	var out []string
	for _, t := range strings.Split(meta["tags"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// noteName is date, a slug of the question (or the note), and a short hash of
// the text, so two notes never share a file.
func (m *Store) noteName(a toolArgs) string {
	src := a.Question
	if strings.TrimSpace(src) == "" {
		src = a.Content
	}
	words := strings.FieldsFunc(strings.ToLower(src), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) > 6 {
		words = words[:6]
	}
	sum := sha256.Sum256([]byte(a.Content + "\x00" + a.Question))
	return fmt.Sprintf("%s-%s-%s.md", m.now().Format("2006-01-02"),
		mcpserver.NormalizeProject(strings.Join(words, "-")), hex.EncodeToString(sum[:3]))
}

// find returns the one note whose text contains phrase, or the one a path
// (strategy:<name>) names.
func (m *Store) find(phrase string) (string, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return "", fmt.Errorf("old_text is required: a phrase copied from the note")
	}
	if kind, name, ok := parseItemPath(phrase); ok && m.keeps(kind) {
		if p := m.Named(kind, name); p != "" {
			return p, nil
		}
		return "", fmt.Errorf("no %s named %q", kind, name)
	}
	var paths []string
	for _, d := range m.dirs() {
		ps, _ := filepath.Glob(filepath.Join(d, "*.md"))
		paths = append(paths, ps...)
	}
	sort.Strings(paths)
	var hits []string
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), phrase) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no note contains %q", phrase)
	case 1:
		return hits[0], nil
	}
	names := make([]string, len(hits))
	for i, p := range hits {
		names[i] = filepath.Base(p)
	}
	return "", fmt.Errorf("%d notes contain %q (%s) — use a longer phrase", len(hits), phrase, strings.Join(names, ", "))
}

// templateToken is a chat-template control token (<|im_start|> and kin).
// In a note it would come back inside a later prompt as a real turn boundary.
var templateToken = regexp.MustCompile(`<\|[a-z_]+\|>`)

// scanNote refuses text that would act on a later prompt instead of informing
// it: invisible format characters (zero-width, bidi overrides — text the
// human reviewing a note cannot see), control characters, template tokens.
func scanNote(content, question string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content is empty")
	}
	if n := len([]rune(content)) + len([]rune(question)); n > memoryMaxChars {
		return fmt.Errorf("note is %d characters; the limit is %d — one fact per note", n, memoryMaxChars)
	}
	for _, s := range []string{content, question} {
		for _, r := range s {
			if unicode.Is(unicode.Cf, r) || (unicode.IsControl(r) && r != '\n' && r != '\t') {
				return fmt.Errorf("note contains an invisible or control character (U+%04X)", r)
			}
		}
		if templateToken.MatchString(s) {
			return fmt.Errorf("note contains a chat-template token (%s)", templateToken.FindString(s))
		}
	}
	if strings.Contains(question, "\n") {
		return fmt.Errorf("question must be one line")
	}
	return nil
}

// NotesFor is the store for a workspace's project: its notes and the
// global ones, indexed into dund's memories indexes through link. A nil link
// keeps the files and indexes nothing.
func NotesFor(link *Link, workspace string) *Store {
	m := &Store{dir: memoryDir(mcpserver.RaglitProject(workspace)), global: memoryDir(globalMemoryDir), now: time.Now,
		index: NotesIndex, kinds: []string{KindMemory}, groups: [2]string{GroupMemory, GroupMemoryGlobal}}
	if link == nil {
		return m
	}
	m.ingest = func(path string) error { return link.ingest(m, path) }
	m.retag = func(path string) error { return link.tag(m, path) }
	m.forget = func(path string) error { return link.forget(m, path) }
	return m
}

// scopeOf is the dund project a note's index belongs to: "" (the global
// set) for a global note, else the project's slug.
func (m *Store) scopeOf(path, project string) string {
	if m.global != "" && filepath.Dir(path) == m.global {
		return ""
	}
	return project
}

// Group names the surfacing group of a hit: memory for a project note in
// this store, memory-global for a global one, "" for anything else.
// Nil-safe: with no store nothing is a note.
func (m *Store) Group(docID string) string {
	switch {
	case m == nil || !m.Holds(docID):
		return ""
	}
	g := m.groups
	if g[0] == "" {
		g = [2]string{GroupMemory, GroupMemoryGlobal}
	}
	if m.global != "" && strings.HasPrefix(docID, m.global+string(os.PathSeparator)) {
		return g[1]
	}
	return g[0]
}

// Holds reports whether path is in one of the store's directories.
func (m *Store) Holds(path string) bool {
	for _, d := range m.dirs() {
		if strings.HasPrefix(path, d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// WithTool handles the memory tool locally; everything else routes onward.
// call parses and applies one call's arguments and returns the result the
// model reads (Stores.Call).
func WithTool(inner agent.ToolDispatcher, call func(rawArgs string) string, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "memory" {
			return inner(ctx, tc)
		}
		out := call(tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall("memory", a, out)
		}
		return out, nil
	}
}

// Call parses and applies one memory call on m (the notes store), rendering
// any error as the result the model reads. A strategy or a constraint goes
// to the strategies store.
func (s Stores) Call(m *Store, rawArgs string, meta NoteMeta) string {
	var a toolArgs
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return "memory: arguments are not valid JSON: " + err.Error()
	}
	// A strategy or a constraint is in the strategies store: by its kind,
	// or by the path old_text names.
	kind := a.Kind
	if k, _, ok := parseItemPath(a.OldText); ok && kind == "" {
		kind = k
	}
	if kind == KindStrategy || kind == KindConstraint {
		if m = s.Strategies; m == nil {
			return "memory: this session keeps no strategies or constraints"
		}
	}
	// The candidate tag is the learning phase's; the user accepts or
	// rejects a candidate (lesson.go).
	for _, t := range a.Tags {
		if strings.EqualFold(strings.TrimSpace(t), CandidateTag) {
			return "memory: the candidate tag is set by the learning phase only; the user reviews candidates with /memories candidates"
		}
	}
	out, err := m.apply(a, meta)
	if err != nil {
		return "memory: " + err.Error()
	}
	return out
}
