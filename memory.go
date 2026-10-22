package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Memory — notes a later session on this project needs and cannot get from the
// code, the git history or the docs (plan item 9).
//
// A note is one markdown file in ~/.dun/memory/<project>/, put into the
// `memory` index of the project's raglit namespace. That index is the reason a
// note is worth writing: the proactive search (docsPreparer) covers every index
// in the namespace, so a note comes back into a later conversation unasked,
// inlined whole, when that conversation is about it (M0: right note on 10 of
// 11 probes that share its words, 0 wrong). So the tool exists only while the
// docs server runs — without the search, a note is never read again.
//
// Writes go straight to disk, with no approval step (USER, 2026-10-19), after
// a scan for text that would act on a later prompt instead of informing it.
// A note is found again by a phrase from it, not by an id: a model that has to
// carry ids between calls invents them (the exec_2 lesson in the icebox).
//
// Local only for now (USER, 2026-10-21). `public` is recorded in the
// frontmatter and changes nothing yet; the remote private/public layers are a
// later slice that uploads these same files.

// memoryIndex is the raglit index notes go into, in the project's namespace.
const memoryIndex = "memory"

// memoryMaxChars caps a note's text, so the whole file (with frontmatter)
// stays under inlineMaxChars and surfaces inline instead of as a pointer.
const memoryMaxChars = 1200

// memoryDir is where a project's notes live. $DUN_HOME overrides ~/.dun, as
// for sessions.
func memoryDir(project string) string {
	return filepath.Join(filepath.Dir(SessionsDir()), "memory", project)
}

func memoryToolDef() llm.ToolDef {
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
		"phrase copied from it or its question."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"add", "refine", "void", "confirm"}},
			"content":  map[string]any{"type": "string", "description": "add/refine: the note — the fact and where it applies, and why it holds only if that was said; never add a reason or consequence nobody stated"},
			"question": map[string]any{"type": "string", "description": "add/refine: the question this note answers, in the words a later user would ask it"},
			"old_text": map[string]any{"type": "string", "description": "refine/void/confirm: a phrase copied exactly from the note or its question"},
			"why":      map[string]any{"type": "string", "description": "refine/void/confirm: what the user said, or what in the code shows it (a file and what it says)"},
			"source":   map[string]any{"type": "string", "enum": []string{"user", "code"}, "description": "refine/void/confirm: who says so — the user, or the code as it is now"},
			"public":   map[string]any{"type": "boolean", "description": "add: share with everyone on this project (default: private to you)"},
		},
		"required": []string{"action"},
	}
	return td
}

type memoryArgs struct {
	Action   string `json:"action"`
	Content  string `json:"content"`
	Question string `json:"question"`
	OldText  string `json:"old_text"`
	Why      string `json:"why"`
	Source   string `json:"source"`
	Public   bool   `json:"public"`
}

// noteMeta is what the frontmatter records about where a note came from.
type noteMeta struct {
	Session string
	Commit  string
	// extra is a rewritten note's existing frontmatter, kept over the above.
	extra map[string]string
}

// memoryStore writes, replaces and removes notes in one directory, and keeps
// the index in step. ingest and forget are nil when there is no index.
type memoryStore struct {
	dir    string
	ingest func(path string) error
	forget func(path string) error
	now    func() time.Time
}

// apply runs one memory call. The error is for the model to read: a bad call
// is answered, never fatal.
func (m *memoryStore) apply(a memoryArgs, meta noteMeta) (string, error) {
	switch a.Action {
	case "add":
		if err := scanNote(a.Content, a.Question); err != nil {
			return "", err
		}
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return "", err
		}
		path := filepath.Join(m.dir, m.noteName(a))
		if err := m.write(path, a, meta); err != nil {
			return "", err
		}
		return "saved " + filepath.Base(path), nil
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
		a.Public = old.meta["public"] == "true"
		if err := m.attest(path, attestEntry{Kind: "corrected", Unit: old.unit(), Text: strings.TrimSpace(a.Content),
			Note: a.Why, By: by, Was: old.body}); err != nil {
			return "", err
		}
		// Same file, so the index replaces the document instead of adding one.
		old.meta["refined"] = m.now().Format("2006-01-02")
		delete(old.meta, "confirmed") // a confirmation was of the text this replaces
		if err := m.write(path, a, noteMeta{Session: meta.Session, Commit: meta.Commit, extra: old.meta}); err != nil {
			return "", err
		}
		return "refined " + filepath.Base(path) + " (the old text is kept in its log)", nil
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
		// unsupported, not retract: in attest, retract withdraws an earlier
		// ruling and leaves the unit untouched; unsupported is the one
		// verdict that stops a claim counting.
		if err := m.attest(path, attestEntry{Kind: "unsupported", Unit: old.unit(), Note: a.Why, By: by}); err != nil {
			return "", err
		}
		if m.forget != nil {
			if err := m.forget(path); err != nil {
				return "", fmt.Errorf("not voided: the index refused it: %w", err)
			}
		}
		// Kept, out of the *.md set that is listed and indexed: nothing a
		// note said is destroyed.
		if err := os.Rename(path, path+voidSuffix); err != nil {
			return "", err
		}
		return "voided " + filepath.Base(path) + " (kept as " + filepath.Base(path+voidSuffix) + ")", nil
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
		if err := m.attest(path, attestEntry{Kind: "confirmed", Unit: old.unit(), Note: a.Why, By: by}); err != nil {
			return "", err
		}
		old.meta["confirmed"] = m.now().Format("2006-01-02")
		if err := m.write(path, memoryArgs{Content: old.body, Question: old.meta["question"], Public: old.meta["public"] == "true"},
			noteMeta{extra: old.meta}); err != nil {
			return "", err
		}
		return "confirmed " + filepath.Base(path), nil
	default:
		return "", fmt.Errorf("action must be add, refine, void or confirm, not %q", a.Action)
	}
}

func (m *memoryStore) write(path string, a memoryArgs, meta noteMeta) error {
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
	fmt.Fprintf(&b, "public: %t\n", a.Public)
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(a.Content) + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if m.ingest != nil {
		if err := m.ingest(path); err != nil {
			// The file is the record; the index can be rebuilt from it. Say so
			// rather than fail the call, or the model retries a write that
			// already happened.
			log.Printf("dun: memory: %s saved but not indexed: %v", filepath.Base(path), err)
		}
	}
	return nil
}

// noteName is date, a slug of the question (or the note), and a short hash of
// the text, so two notes never share a file.
func (m *memoryStore) noteName(a memoryArgs) string {
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
		normalizeProject(strings.Join(words, "-")), hex.EncodeToString(sum[:3]))
}

// find returns the one note whose text contains phrase.
func (m *memoryStore) find(phrase string) (string, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return "", fmt.Errorf("old_text is required: a phrase copied from the note")
	}
	paths, _ := filepath.Glob(filepath.Join(m.dir, "*.md"))
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

// raglitMemory is the store for a workspace's project, indexed through the
// docs server's own raglit binary and store flags, so a note lands where that
// server searches (the shared daemon by default; an --embedded --home when the
// config says so).
func raglitMemory(docs Server, workspace string) *memoryStore {
	m := &memoryStore{dir: memoryDir(raglitProject(workspace)), now: time.Now}
	if filepath.Base(docs.Command) != "raglit" {
		return m // not raglit: keep the file, index nothing
	}
	store := raglitStoreFlags(docs.Args)
	run := func(args ...string) error {
		out, err := detach(exec.Command(docs.Command, args...)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	m.ingest = func(path string) error {
		return run(append(append([]string{"ingest"}, store...), "--index", memoryIndex, "--now", path)...)
	}
	m.forget = func(path string) error {
		return run(append(append([]string{"forget"}, store...), "--index", memoryIndex, path)...)
	}
	return m
}

// raglitStoreFlags picks the flags that choose WHICH store out of a raglit
// serve command line: --embedded, --home, --daemon, --project. --index is
// left out — notes have their own.
func raglitStoreFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			continue
		}
		switch name {
		case "embedded":
			out = append(out, a)
		case "home", "daemon", "project":
			out = append(out, a)
			if !hasValue && i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
		}
	}
	return out
}

// group names the surfacing group of a hit: memory for a note in this store,
// "" for anything else. Nil-safe: with no store nothing is a note.
func (m *memoryStore) group(docID string) string {
	if m != nil && strings.HasPrefix(docID, m.dir+string(os.PathSeparator)) {
		return groupMemory
	}
	return ""
}

// memoryStore is the note store, or nil when the docs server is not running.
func (h *Harness) memoryStore() *memoryStore { return h.memory.Load() }

// withMemory handles the memory tool locally; everything else routes onward.
func withMemory(inner agent.ToolDispatcher, h *Harness, m *memoryStore, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "memory" {
			return inner(ctx, tc)
		}
		out := h.memoryCall(m, tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall("memory", a, out)
		}
		return out, nil
	}
}

// memoryCall parses and applies one memory call, rendering any error as the
// result the model reads.
func (h *Harness) memoryCall(m *memoryStore, rawArgs string) string {
	var a memoryArgs
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return "memory: arguments are not valid JSON: " + err.Error()
	}
	meta := noteMeta{Commit: gitLine(h.cfg.Workspace, "rev-parse", "--short", "HEAD")}
	if h.cfg.SessionFile != "" {
		meta.Session = strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
	}
	out, err := m.apply(a, meta)
	if err != nil {
		return "memory: " + err.Error()
	}
	return out
}
