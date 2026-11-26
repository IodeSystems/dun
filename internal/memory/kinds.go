package memory

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunhome"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// Memories, strategies and constraints: one file shape, three kinds
// (docs/strategies.md).
//
// Each is a markdown file with frontmatter (name, kind, important, tags,
// date, ...) and a body, its detail. Scope is the directory: the project's
// or @global. A memory is a fact (memory.go); a strategy is an approach
// that worked; a constraint is a rule the user set for how to work. The
// rules FILES (AGENTS.md, CLAUDE.md and their imports, rules.go) are a
// different thing and are not touched here.
//
// Memories go into dund's memories index; strategies and constraints into
// its strategies index (four indexes, docs/rag.md), told apart by kind.
// Both stores are Store over their own directories, with the same
// write, attest, sync and recall code.
//
// important: the user marks an item (/memories important); it is then
// listed by path in the system prompt of every session, with no retrieval,
// up to ImportantCap across the three kinds. The model cannot mark one:
// a model-written list on every request was refused for memory
// (docs/memory.md, Personalization, USER 2026-10-19). Its detail is a
// file_read of its path (memory:<name>, strategy:<name>, constraint:<name>).
//
// Learning phase (lesson.go): a resolved churn becomes a candidate strategy
// through strategyStore().apply with kind strategy, as the memory tool's
// add does, tagged candidate until the user accepts it.

const (
	KindMemory     = "memory"
	KindStrategy   = "strategy"
	KindConstraint = "constraint"

	// StrategiesIndex is dund's index of strategies and constraints.
	StrategiesIndex = "strategies"

	// GroupMemory is the group of hits that are this project's memory
	// notes, GroupMemoryGlobal of the global ones (scope global). Separate
	// groups: the two are separate indexes, whose bm25 scores do not
	// compare.
	GroupMemory       = "memory"
	GroupMemoryGlobal = "memory-global"

	// GroupStrategy and GroupStrategyGlobal are the surfacing groups of a
	// project's and a global strategy or constraint.
	GroupStrategy       = "strategy"
	GroupStrategyGlobal = "strategy-global"

	// ImportantCap is how many important items the standing list shows,
	// the three kinds together: constraints first, then strategies, then
	// memories; project before global; by name. One past it is not listed;
	// it stays in its index and surfaces by retrieval like the rest.
	ImportantCap = 12

	// itemNameMax caps a name, in bytes of its slug.
	itemNameMax = 48
)

// strategiesDir is where a project's strategies and constraints live
// (globalMemoryDir for the global ones).
func strategiesDir(project string) string {
	return filepath.Join(dunhome.Dir(), "strategies", project)
}

// StrategiesFor is the strategies and constraints store of a workspace's
// project and the global set, indexed through link (nil: files only). The
// directory is keyed by the registry id, as NotesFor keys memory, so a
// moved repo and same-named clones keep their own; the old name-keyed set is
// copied in once.
func StrategiesFor(link *Link, workspace string) *Store {
	id := projectKey(workspace)
	m := &Store{dir: strategiesDir(id), global: strategiesDir(globalMemoryDir), now: time.Now,
		index: StrategiesIndex, kinds: []string{KindStrategy, KindConstraint}, groups: [2]string{GroupStrategy, GroupStrategyGlobal}}
	if legacy := mcpserver.RaglitProject(workspace); legacy != id {
		m.legacy = strategiesDir(legacy)
		migrateFrom(m.legacy, m.dir)
	}
	if link == nil {
		return m
	}
	m.ingest = func(path string) error { return link.ingest(m, path) }
	m.retag = func(path string) error { return link.tag(m, path) }
	m.forget = func(path string) error { return link.forget(m, path) }
	return m
}

func (m *Store) indexName() string { return cmp.Or(m.index, NotesIndex) }

func (m *Store) kindList() []string {
	if len(m.kinds) == 0 {
		return []string{KindMemory}
	}
	return m.kinds
}

func (m *Store) keeps(kind string) bool {
	for _, k := range m.kindList() {
		if k == kind {
			return true
		}
	}
	return false
}

// kindOf is the kind a call asks for, "" being the store's first.
func (m *Store) kindOf(k string) (string, error) {
	if k == "" {
		return m.kindList()[0], nil
	}
	if !m.keeps(k) {
		return "", fmt.Errorf("kind must be %s, not %q", strings.Join(m.kindList(), " or "), k)
	}
	return k, nil
}

// scanItem is scanNote for any kind: a strategy or constraint needs a name
// and may have no detail.
func scanItem(a toolArgs) error {
	if a.Kind == KindMemory || a.Kind == "" {
		return scanNote(a.Content, a.Question)
	}
	if itemSlug(a.Name) == "" {
		return fmt.Errorf("name is required for a %s: a few words", a.Kind)
	}
	return scanNote(cmp.Or(strings.TrimSpace(a.Content), a.Name), a.Question)
}

// itemSlug is a name as a path takes it: lower case, letters and digits,
// one '-' between words, at most itemNameMax bytes.
func itemSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	out := cutUTF8(b.String(), itemNameMax)
	return strings.Trim(out, "-")
}

// newPath is the file a new item is written to, and sets a.Name to the
// name it is found by. A memory keeps its dated file name; its name is the
// one given, else its question's first words, made unique. A strategy or
// constraint is <kind>-<name>.md, and a name its scope already has is
// refused.
func (m *Store) newPath(dir string, a *toolArgs) (string, error) {
	if a.Kind == KindMemory || a.Kind == "" {
		name := itemSlug(a.Name)
		if name == "" {
			words := strings.Fields(itemSlugWords(cmp.Or(strings.TrimSpace(a.Question), a.Content)))
			if len(words) > 6 {
				words = words[:6]
			}
			name = cmp.Or(strings.Join(words, "-"), "note")
		}
		if m.Named(KindMemory, name) != "" {
			sum := sha256.Sum256([]byte(a.Content + "\x00" + a.Question))
			name += "-" + hex.EncodeToString(sum[:3])
		}
		a.Name = name
		return filepath.Join(dir, m.noteName(*a)), nil
	}
	name := itemSlug(a.Name)
	if name == "" {
		return "", fmt.Errorf("name is required for a %s: a few words", a.Kind)
	}
	a.Name = name
	if namedIn(dir, a.Kind, name) != "" {
		return "", fmt.Errorf("%s:%s exists: refine it (old_text %q), or void it first", a.Kind, name, a.Kind+":"+name)
	}
	return filepath.Join(dir, a.Kind+"-"+name+".md"), nil
}

// itemSlugWords is s's words, space-separated, as itemSlug spells them.
func itemSlugWords(s string) string {
	return strings.ReplaceAll(itemSlug(s), "-", " ")
}

// noteKind is a file's kind, from its frontmatter: memory when unset.
func noteKind(meta map[string]string) string { return cmp.Or(meta["kind"], KindMemory) }

// noteNameOf is a file's name: its frontmatter's, else (a note written
// before names) its file name without .md.
func noteNameOf(path string, meta map[string]string) string {
	return cmp.Or(meta["name"], strings.TrimSuffix(filepath.Base(path), ".md"))
}

// noteTitle is what a hit is called: a note's question, else its path.
func noteTitle(path string, meta map[string]string) string {
	return cmp.Or(meta["question"], noteKind(meta)+":"+noteNameOf(path, meta))
}

// parseItemPath splits memory:<name>, strategy:<name> or constraint:<name>.
func parseItemPath(s string) (kind, name string, ok bool) {
	k, n, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found || n == "" || strings.ContainsAny(n, " \t\n/") {
		return "", "", false
	}
	switch k {
	case KindMemory, KindStrategy, KindConstraint:
		return k, n, true
	}
	return "", "", false
}

// Named is the file of the item kind:name, the project's before the
// global one; "" when there is none.
func (m *Store) Named(kind, name string) string {
	if m == nil {
		return ""
	}
	for _, d := range m.dirs() {
		if p := namedIn(d, kind, name); p != "" {
			return p
		}
	}
	return ""
}

func namedIn(dir, kind, name string) string {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	sort.Strings(paths)
	for _, p := range paths {
		n, err := readNote(p)
		if err == nil && noteKind(n.meta) == kind && noteNameOf(p, n.meta) == name {
			return p
		}
	}
	return ""
}

// names lists a kind's names, for a "no such item" answer.
func (m *Store) names(kind string) []string {
	var out []string
	for _, i := range m.List() {
		if !i.Void && i.Kind == kind {
			out = append(out, i.Kind+":"+i.Name)
		}
	}
	return out
}

// Stores is a session's two stores: the memory notes, and the strategies
// and constraints. Either is nil when the session keeps none.
type Stores struct {
	Notes, Strategies *Store
}

// For is the store that keeps kind, nil when the session keeps none.
func (s Stores) For(kind string) *Store {
	if kind == KindMemory {
		return s.Notes
	}
	return s.Strategies
}

// importantItems is the important, current items of the stores, in the
// standing list's order.
func importantItems(stores ...*Store) []Info {
	var out []Info
	for _, s := range stores {
		if s == nil {
			continue
		}
		for _, i := range s.List() {
			if i.Important && !i.Void {
				out = append(out, i)
			}
		}
	}
	rank := map[string]int{KindConstraint: 0, KindStrategy: 1, KindMemory: 2}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if rank[x.Kind] != rank[y.Kind] {
			return rank[x.Kind] < rank[y.Kind]
		}
		if x.Global != y.Global {
			return !x.Global
		}
		return x.Name < y.Name
	})
	return out
}

func (s Stores) important() []Info {
	return importantItems(s.Strategies, s.Notes)
}

// ImportantKey names the important set, to tell when it changed.
func (s Stores) ImportantKey() string {
	var b strings.Builder
	for _, i := range s.important() {
		b.WriteString(i.path + "\x00")
	}
	return b.String()
}

// Standing is the system prompt's list of important items, "" when none,
// and the files it lists (which recall then leaves out).
func (s Stores) Standing() (string, map[string]bool) {
	items := s.important()
	listed := map[string]bool{}
	if len(items) == 0 {
		return "", listed
	}
	var b strings.Builder
	b.WriteString("\n\n## Standing constraints, strategies and notes\n\n" +
		"The user marked these important. Each holds in every session here (\"this project\" or \"every project\"). " +
		"Read one's detail with file_read on its path before you act on it.")
	for n, i := range items {
		if n == ImportantCap {
			fmt.Fprintf(&b, "\n(%d more marked important are not listed: the list holds %d. They are found by search like the rest.)",
				len(items)-ImportantCap, ImportantCap)
			break
		}
		where := "this project"
		if i.Global {
			where = "every project"
		}
		fmt.Fprintf(&b, "\n- %s:%s (%s)", i.Kind, i.Name, where)
		listed[i.path] = true
	}
	return b.String(), listed
}

// WithItemRead answers file_read on memory:<name>, strategy:<name> and
// constraint:<name> with that item's file; any other path goes on to the
// files server. Only these prefixes (docs/reading.md): an item is small,
// so there are no ranges or caps to apply. stores is read at each call.
func WithItemRead(inner agent.ToolDispatcher, stores func() Stores) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		if tc.Function.Name != "file_read" {
			return inner(ctx, tc)
		}
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
		kind, name, ok := parseItemPath(a.Path)
		if !ok {
			return inner(ctx, tc)
		}
		return llm.TextResult(stores().readItem(kind, name)), nil
	}
}

func (s Stores) readItem(kind, name string) string {
	m := s.For(kind)
	if m == nil {
		return "ERROR: memories, strategies and constraints are off in this session"
	}
	p := m.Named(kind, name)
	if p == "" {
		known := m.names(kind)
		if len(known) == 0 {
			return fmt.Sprintf("ERROR: no %s named %q; there are none", kind, name)
		}
		return fmt.Sprintf("ERROR: no %s named %q; there are: %s", kind, name, strings.Join(known, ", "))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return kind + ":" + name + "\n" + string(b)
}

// Usage is /memories' usage line.
const Usage = "usage: /memories [add memory|strategy|constraint [global] <name> [detail] | rm <kind:name> | important|unimportant <kind:name> | candidates | accept|reject <kind:name>]"

// Command carries out /memories add, rm, important, unimportant,
// candidates, accept and reject, and returns the line for the user. The
// user is the source: an rm is a void with the user as the evidence. meta
// is where a write from this session says it came from.
func (s Stores) Command(action string, meta NoteMeta) (string, error) {
	f := strings.Fields(action)
	if len(f) == 1 && f[0] == "candidates" {
		return s.candidatesListing()
	}
	if len(f) < 2 {
		return "", fmt.Errorf("%s", Usage)
	}
	switch f[0] {
	case "add":
		kind, rest := f[1], f[2:]
		m := s.For(kind)
		if kind != KindMemory && kind != KindStrategy && kind != KindConstraint {
			return "", fmt.Errorf("%s", Usage)
		}
		if m == nil {
			return "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
		}
		a := toolArgs{Action: "add", Kind: kind}
		if len(rest) > 0 && (rest[0] == "global" || rest[0] == "--global") {
			a.Scope, rest = "global", rest[1:]
		}
		if len(rest) == 0 {
			return "", fmt.Errorf("%s", Usage)
		}
		a.Name, a.Content = rest[0], strings.Join(rest[1:], " ")
		if kind == KindMemory {
			a.Question = strings.ReplaceAll(rest[0], "-", " ")
		}
		dir := m.dir
		if a.Scope == "global" && m.global != "" {
			dir = m.global
		}
		// The user's add replaces an item of the same kind, scope and name
		// (USER 2026-11-08); the model's add refuses one.
		if p := namedIn(dir, kind, itemSlug(a.Name)); p != "" {
			return m.replace(p, a, meta)
		}
		return m.apply(a, meta)
	case "rm":
		m, _, err := s.itemArg(f[1])
		if err != nil {
			return "", err
		}
		return m.apply(toolArgs{Action: "void", OldText: f[1], Why: "removed by the user with /memories rm", Source: "user"}, meta)
	case "accept", "reject":
		return s.reviewCandidate(f[0], f[1], meta)
	case "important", "unimportant":
		m, path, err := s.itemArg(f[1])
		if err != nil {
			return "", err
		}
		if f[0] == "important" && IsCandidate(path) {
			return "", fmt.Errorf("%s is a candidate: /memories accept it first", f[1])
		}
		n, err := readNote(path)
		if err != nil {
			return "", err
		}
		if f[0] == "important" {
			n.meta["important"] = "true"
		} else {
			delete(n.meta, "important")
		}
		down, err := m.write(path, toolArgs{Content: n.body, Question: n.meta["question"], Public: n.meta["public"] == "true",
			Tags: noteTags(n.meta)}, NoteMeta{extra: n.meta, tagOnly: true})
		if err != nil {
			return "", err
		}
		out := f[1] + ": " + f[0]
		if f[0] == "important" {
			out += ", in the standing list of every session"
			if items := s.important(); len(items) > ImportantCap {
				out += fmt.Sprintf(" — %d are important; the list holds %d (constraints, then strategies, then memories; project first; by name), the rest are found by search", len(items), ImportantCap)
			}
		}
		return out + down, nil
	}
	return "", fmt.Errorf("%s", Usage)
}

// itemArg resolves a kind:name argument to its store and file.
func (s Stores) itemArg(arg string) (*Store, string, error) {
	kind, name, ok := parseItemPath(arg)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a path: memory:<name>, strategy:<name> or constraint:<name>", arg)
	}
	m := s.For(kind)
	if m == nil {
		return nil, "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
	}
	p := m.Named(kind, name)
	if p == "" {
		return nil, "", fmt.Errorf("no %s named %q", kind, name)
	}
	return m, p, nil
}

// replace rewrites the item at path with a's detail, for the user's
// /memories add of a name that exists. The old detail goes into the
// attestation log (corrected, was), the file keeps its name, date and
// important flag, and it is indexed again.
func (m *Store) replace(path string, a toolArgs, meta NoteMeta) (string, error) {
	if err := scanItem(a); err != nil {
		return "", err
	}
	old, err := readNote(path)
	if err != nil {
		return "", err
	}
	if err := m.attest(path, AttestEntry{Kind: "corrected", Unit: old.unit(), Text: strings.TrimSpace(a.Content),
		Note: "replaced by the user with /memories add", By: "user", Was: old.body}); err != nil {
		return "", err
	}
	old.meta["refined"] = m.now().Format("2006-01-02")
	delete(old.meta, "confirmed")
	a.Question = cmp.Or(old.meta["question"], a.Question)
	a.Tags = noteTags(old.meta)
	a.Public = old.meta["public"] == "true"
	down, err := m.write(path, a, NoteMeta{Session: meta.Session, Commit: meta.Commit, extra: old.meta})
	if err != nil {
		return "", err
	}
	return "replaced " + noteKind(old.meta) + ":" + noteNameOf(path, old.meta) + " (the old detail is kept in its log)" + down, nil
}

// cutUTF8 is s cut to at most n bytes, on a rune boundary (harness
// rules.go has its twin).
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
