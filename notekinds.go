package dun

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

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
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
// Both stores are memoryStore over their own directories, with the same
// write, attest, sync and recall code.
//
// important: the user marks an item (/memories important); it is then
// listed by path in the system prompt of every session, with no retrieval,
// up to importantCap across the three kinds. The model cannot mark one:
// a model-written list on every request was refused for memory
// (docs/memory.md, Personalization, USER 2026-10-19). Its detail is a
// file_read of its path (memory:<name>, strategy:<name>, constraint:<name>).
//
// Learning phase (not built, docs/rag.md): a resolved churn becomes a
// strategy through strategyStore().apply with kind strategy, as the memory
// tool's add does.

const (
	kindMemory     = "memory"
	kindStrategy   = "strategy"
	kindConstraint = "constraint"

	// strategiesIndex is dund's index of strategies and constraints.
	strategiesIndex = "strategies"

	// groupStrategy and groupStrategyGlobal are the surfacing groups of a
	// project's and a global strategy or constraint.
	groupStrategy       = "strategy"
	groupStrategyGlobal = "strategy-global"

	// importantCap is how many important items the standing list shows,
	// the three kinds together: constraints first, then strategies, then
	// memories; project before global; by name. One past it is not listed;
	// it stays in its index and surfaces by retrieval like the rest.
	importantCap = 12

	// itemNameMax caps a name, in bytes of its slug.
	itemNameMax = 48
)

// strategiesDir is where a project's strategies and constraints live
// (globalMemoryDir for the global ones).
func strategiesDir(project string) string {
	return filepath.Join(filepath.Dir(SessionsDir()), "strategies", project)
}

// strategyFor is the strategies and constraints store of a workspace's
// project and the global set, indexed through link (nil: files only).
func strategyFor(link *memLink, workspace string) *memoryStore {
	m := &memoryStore{dir: strategiesDir(raglitProject(workspace)), global: strategiesDir(globalMemoryDir), now: time.Now,
		index: strategiesIndex, kinds: []string{kindStrategy, kindConstraint}, groups: [2]string{groupStrategy, groupStrategyGlobal}}
	if link == nil {
		return m
	}
	m.ingest = func(path string) error { return link.ingest(m, path) }
	m.forget = func(path string) error { return link.forget(m, path) }
	return m
}

func (m *memoryStore) indexName() string { return cmp.Or(m.index, memoryIndex) }

func (m *memoryStore) kindList() []string {
	if len(m.kinds) == 0 {
		return []string{kindMemory}
	}
	return m.kinds
}

func (m *memoryStore) keeps(kind string) bool {
	for _, k := range m.kindList() {
		if k == kind {
			return true
		}
	}
	return false
}

// kindOf is the kind a call asks for, "" being the store's first.
func (m *memoryStore) kindOf(k string) (string, error) {
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
func scanItem(a memoryArgs) error {
	if a.Kind == kindMemory || a.Kind == "" {
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
func (m *memoryStore) newPath(dir string, a *memoryArgs) (string, error) {
	if a.Kind == kindMemory || a.Kind == "" {
		name := itemSlug(a.Name)
		if name == "" {
			words := strings.Fields(itemSlugWords(cmp.Or(strings.TrimSpace(a.Question), a.Content)))
			if len(words) > 6 {
				words = words[:6]
			}
			name = cmp.Or(strings.Join(words, "-"), "note")
		}
		if m.named(kindMemory, name) != "" {
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
func noteKind(meta map[string]string) string { return cmp.Or(meta["kind"], kindMemory) }

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
	case kindMemory, kindStrategy, kindConstraint:
		return k, n, true
	}
	return "", "", false
}

// named is the file of the item kind:name, the project's before the
// global one; "" when there is none.
func (m *memoryStore) named(kind, name string) string {
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
func (m *memoryStore) names(kind string) []string {
	var out []string
	for _, i := range m.list() {
		if !i.Void && i.Kind == kind {
			out = append(out, i.Kind+":"+i.Name)
		}
	}
	return out
}

func (h *Harness) strategyStore() *memoryStore { return h.strategies.Load() }

// storeFor is the store that keeps kind, nil when the session keeps none.
func (h *Harness) storeFor(kind string) *memoryStore {
	if kind == kindMemory {
		return h.memoryStore()
	}
	return h.strategyStore()
}

// importantItems is the important, current items of the stores, in the
// standing list's order.
func importantItems(stores ...*memoryStore) []MemoryInfo {
	var out []MemoryInfo
	for _, s := range stores {
		if s == nil {
			continue
		}
		for _, i := range s.list() {
			if i.Important && !i.Void {
				out = append(out, i)
			}
		}
	}
	rank := map[string]int{kindConstraint: 0, kindStrategy: 1, kindMemory: 2}
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

func (h *Harness) important() []MemoryInfo {
	return importantItems(h.strategyStore(), h.memoryStore())
}

// importantKey names the important set, to tell when it changed.
func (h *Harness) importantKey() string {
	var b strings.Builder
	for _, i := range h.important() {
		b.WriteString(i.path + "\x00")
	}
	return b.String()
}

// standingBlock is the system prompt's list of important items, and sets
// the files it lists (which recall then leaves out). "" when none.
func (h *Harness) standingBlock() string {
	items := h.important()
	listed := map[string]bool{}
	defer h.listed.Store(&listed)
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Standing constraints, strategies and notes\n\n" +
		"The user marked these important. Each holds in every session here (\"this project\" or \"every project\"). " +
		"Read one's detail with file_read on its path before you act on it.")
	for n, i := range items {
		if n == importantCap {
			fmt.Fprintf(&b, "\n(%d more marked important are not listed: the list holds %d. They are found by search like the rest.)",
				len(items)-importantCap, importantCap)
			break
		}
		where := "this project"
		if i.Global {
			where = "every project"
		}
		fmt.Fprintf(&b, "\n- %s:%s (%s)", i.Kind, i.Name, where)
		listed[i.path] = true
	}
	return b.String()
}

// listedImportant says the standing list shows path.
func (h *Harness) listedImportant(path string) bool {
	if l := h.listed.Load(); l != nil {
		return (*l)[path]
	}
	return false
}

// withItemRead answers file_read on memory:<name>, strategy:<name> and
// constraint:<name> with that item's file; any other path goes on to the
// files server. Only these prefixes (docs/reading.md): an item is small,
// so there are no ranges or caps to apply.
func withItemRead(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
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
		return h.readItem(kind, name), nil
	}
}

func (h *Harness) readItem(kind, name string) string {
	m := h.storeFor(kind)
	if m == nil {
		return "ERROR: memories, strategies and constraints are off in this session"
	}
	p := m.named(kind, name)
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

// noteMeta is where a write from this session says it came from.
func (h *Harness) noteMeta() noteMeta {
	meta := noteMeta{Commit: gitLine(h.cfg.Workspace, "rev-parse", "--short", "HEAD")}
	if h.cfg.SessionFile != "" {
		meta.Session = strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
	}
	return meta
}

// MemoriesUsage is /memories' usage line.
const MemoriesUsage = "usage: /memories [add memory|strategy|constraint [global] <name> [detail] | rm <kind:name> | important|unimportant <kind:name>]"

// MemoriesCommand carries out /memories add, rm, important and
// unimportant, and returns the line for the user. The user is the source:
// an rm is a void with the user as the evidence.
func (h *Harness) MemoriesCommand(action string) (string, error) {
	f := strings.Fields(action)
	if len(f) < 2 {
		return "", fmt.Errorf("%s", MemoriesUsage)
	}
	before := h.importantKey()
	defer func() {
		if h.importantKey() != before {
			h.applyTools()
		}
	}()
	switch f[0] {
	case "add":
		kind, rest := f[1], f[2:]
		m := h.storeFor(kind)
		if kind != kindMemory && kind != kindStrategy && kind != kindConstraint {
			return "", fmt.Errorf("%s", MemoriesUsage)
		}
		if m == nil {
			return "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
		}
		a := memoryArgs{Action: "add", Kind: kind}
		if len(rest) > 0 && (rest[0] == "global" || rest[0] == "--global") {
			a.Scope, rest = "global", rest[1:]
		}
		if len(rest) == 0 {
			return "", fmt.Errorf("%s", MemoriesUsage)
		}
		a.Name, a.Content = rest[0], strings.Join(rest[1:], " ")
		if kind == kindMemory {
			a.Question = strings.ReplaceAll(rest[0], "-", " ")
		}
		dir := m.dir
		if a.Scope == "global" && m.global != "" {
			dir = m.global
		}
		// The user's add replaces an item of the same kind, scope and name
		// (USER 2026-11-08); the model's add refuses one.
		if p := namedIn(dir, kind, itemSlug(a.Name)); p != "" {
			return m.replace(p, a, h.noteMeta())
		}
		return m.apply(a, h.noteMeta())
	case "rm":
		m, _, err := h.itemArg(f[1])
		if err != nil {
			return "", err
		}
		return m.apply(memoryArgs{Action: "void", OldText: f[1], Why: "removed by the user with /memories rm", Source: "user"}, h.noteMeta())
	case "important", "unimportant":
		m, path, err := h.itemArg(f[1])
		if err != nil {
			return "", err
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
		down, err := m.write(path, memoryArgs{Content: n.body, Question: n.meta["question"], Public: n.meta["public"] == "true",
			Tags: noteTags(n.meta)}, noteMeta{extra: n.meta})
		if err != nil {
			return "", err
		}
		out := f[1] + ": " + f[0]
		if f[0] == "important" {
			out += ", in the standing list of every session"
			if items := h.important(); len(items) > importantCap {
				out += fmt.Sprintf(" — %d are important; the list holds %d (constraints, then strategies, then memories; project first; by name), the rest are found by search", len(items), importantCap)
			}
		}
		return out + down, nil
	}
	return "", fmt.Errorf("%s", MemoriesUsage)
}

// itemArg resolves a kind:name argument to its store and file.
func (h *Harness) itemArg(s string) (*memoryStore, string, error) {
	kind, name, ok := parseItemPath(s)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a path: memory:<name>, strategy:<name> or constraint:<name>", s)
	}
	m := h.storeFor(kind)
	if m == nil {
		return nil, "", fmt.Errorf("off in this session (DUN_MEMORY=0, or no dund link)")
	}
	p := m.named(kind, name)
	if p == "" {
		return nil, "", fmt.Errorf("no %s named %q", kind, name)
	}
	return m, p, nil
}

// replace rewrites the item at path with a's detail, for the user's
// /memories add of a name that exists. The old detail goes into the
// attestation log (corrected, was), the file keeps its name, date and
// important flag, and it is indexed again.
func (m *memoryStore) replace(path string, a memoryArgs, meta noteMeta) (string, error) {
	if err := scanItem(a); err != nil {
		return "", err
	}
	old, err := readNote(path)
	if err != nil {
		return "", err
	}
	if err := m.attest(path, attestEntry{Kind: "corrected", Unit: old.unit(), Text: strings.TrimSpace(a.Content),
		Note: "replaced by the user with /memories add", By: "user", Was: old.body}); err != nil {
		return "", err
	}
	old.meta["refined"] = m.now().Format("2006-01-02")
	delete(old.meta, "confirmed")
	a.Question = cmp.Or(old.meta["question"], a.Question)
	a.Tags = noteTags(old.meta)
	a.Public = old.meta["public"] == "true"
	down, err := m.write(path, a, noteMeta{Session: meta.Session, Commit: meta.Commit, extra: old.meta})
	if err != nil {
		return "", err
	}
	return "replaced " + noteKind(old.meta) + ":" + noteNameOf(path, old.meta) + " (the old detail is kept in its log)" + down, nil
}
