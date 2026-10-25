package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/llm"
)

// The project's rules: the workspace's root AGENTS.md and CLAUDE.md, and the
// files they import, as a system-prompt block.
//
// The root files are standing context (see agents_hook.go): the model has the
// project's rules from the first message, and a system block survives
// compaction. The block is re-read on every system rebuild, so a rule edited
// mid-session is in the next rebuild without a restart.
//
// Imports are Claude Code's: an @path token outside code spans and fenced
// blocks names a file, relative to the file that names it (~ is the home
// directory). Imported files may import more, up to rulesImportDepth hops; a
// file already included is not included again, so a cycle ends. A missing
// file is a one-line note in the block, not an error. To keep prose like
// "ask @carl" from reading as an import, a token must contain a '/' or a '.'.
//
// Both root files are read (USER 2026-10-22: CLAUDE.md as well — an
// an assumption, reversible). When they are one file
// (a symlink) or have the same bytes, the content goes in once.
//
// Imports can be large, and the block rides every request. Past
// rulesOfferTokens the user is told once (per project, per content change)
// that /rules can make them cheaper: "summary" replaces the imports with a
// condensed version the model writes once (cached under the project's state
// dir, keyed by a hash of the imports), "index" puts them in the raglit index
// "rules" so they surface by relevance. The root files stay verbatim in every
// mode. A summary whose sources changed, or an index with /raglit off, falls
// back to full.

const (
	rulesFileMax     = 64 << 10  // one file's share of the block; the rest is cut with a note
	rulesTotalMax    = 256 << 10 // the whole block's
	rulesImportDepth = 5         // import hops from a root file, as Claude Code
	// rulesOfferTokens is the block size past which the user is offered
	// /rules summary or index. 4k tokens is about 16 KiB of prose: a few
	// pages, re-sent on every request of every turn.
	rulesOfferTokens = 4000
	rulesIndex       = "rules"
	rulesStateFile   = "rules.json"
)

// The rules modes.
const (
	RulesFull    = "full"
	RulesSummary = "summary"
	RulesIndex   = "index"
)

// rulesDoc is one file of the rules.
type rulesDoc struct {
	Path string // absolute
	Name string // as shown: relative to the workspace when inside it, else ~/… or absolute
	From string // the Name of the file that imports it; "" for a root file
	Also string // another root file that is the same file
	Text string // the content, after the caps
	Size int    // bytes on disk
	Note string // why it is missing, unread or cut
}

// rulesSet is the root files and, in reading order, what they import. Off
// is the files /context-file switched off (contextfiles.go): out of every
// request, named in the block, and in the rules index when there is one.
type rulesSet struct {
	Roots   []rulesDoc
	Imports []rulesDoc
	Off     []rulesDoc
}

// rulesLoader reads one rulesSet.
type rulesLoader struct {
	ws, home string
	seen     map[string]bool
	left     int
	out      *[]rulesDoc
}

// loadRules reads the workspace's root rule files and their imports.
func loadRules(workspace string) rulesSet {
	var set rulesSet
	if workspace == "" {
		return set
	}
	home, _ := userHomeDir()
	l := &rulesLoader{ws: workspace, home: home, seen: map[string]bool{}, left: rulesTotalMax}
	// The roots first, so an import of one root by the other is "already
	// included", not a second copy.
	type root struct {
		path string
		data []byte
	}
	var roots []root
	for _, names := range [][]string{{"AGENTS.md", "agents.md"}, {"CLAUDE.md"}} {
		for _, n := range names {
			p := filepath.Join(workspace, n)
			if b, err := os.ReadFile(p); err == nil {
				roots = append(roots, root{p, b})
				break
			}
		}
	}
	var kept []root
	for _, r := range roots {
		dup := false
		for i, k := range kept {
			if realPath(k.path) == realPath(r.path) || string(k.data) == string(r.data) {
				set.Roots[i].Also = l.name(r.path)
				dup = true
				break
			}
		}
		l.seen[realPath(r.path)] = true
		if dup {
			continue
		}
		kept = append(kept, r)
		set.Roots = append(set.Roots, l.doc(r.path, "", r.data))
	}
	l.out = &set.Imports
	for _, d := range set.Roots {
		l.imports(d, 1)
	}
	return set
}

// doc makes a rulesDoc of a file's bytes, cut to the caps.
func (l *rulesLoader) doc(path, from string, data []byte) rulesDoc {
	d := rulesDoc{Path: path, Name: l.name(path), From: from, Size: len(data)}
	text := string(data)
	if len(text) > rulesFileMax {
		text = cutUTF8(text, rulesFileMax)
		d.Note = fmt.Sprintf("cut: the first %d KiB of %d KiB (a file's cap is %d KiB)", rulesFileMax>>10, (d.Size+1023)>>10, rulesFileMax>>10)
	}
	if len(text) > l.left {
		text = cutUTF8(text, l.left)
		d.Note = fmt.Sprintf("cut: the rules reached their %d KiB total", rulesTotalMax>>10)
	}
	l.left -= len(text)
	d.Text = text
	return d
}

// imports reads, depth-first, what d imports.
func (l *rulesLoader) imports(d rulesDoc, depth int) {
	for _, raw := range importPaths(d.Text) {
		p := l.resolve(raw, filepath.Dir(d.Path))
		real := realPath(p)
		if l.seen[real] {
			continue // already in the block: a repeat, or a cycle
		}
		l.seen[real] = true
		note := func(s string) {
			*l.out = append(*l.out, rulesDoc{Path: p, Name: l.name(p), From: d.Name, Note: s})
		}
		if depth > rulesImportDepth {
			note(fmt.Sprintf("not read: imports nest deeper than %d", rulesImportDepth))
			continue
		}
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			note("not found")
			continue
		case err != nil:
			note("not read: " + err.Error())
			continue
		case l.left <= 0 && len(data) > 0:
			note(fmt.Sprintf("not read: the rules reached their %d KiB total", rulesTotalMax>>10))
			continue
		}
		nd := l.doc(p, d.Name, data)
		*l.out = append(*l.out, nd)
		l.imports(nd, depth+1)
	}
}

func (l *rulesLoader) resolve(raw, dir string) string {
	switch {
	case raw == "~" || strings.HasPrefix(raw, "~/"):
		return filepath.Join(l.home, strings.TrimPrefix(raw, "~"))
	case filepath.IsAbs(raw):
		return filepath.Clean(raw)
	}
	return filepath.Join(dir, raw)
}

// name is how a path is shown: relative to the workspace, else to home.
func (l *rulesLoader) name(p string) string {
	if r, err := filepath.Rel(l.ws, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	if l.home != "" {
		if r, err := filepath.Rel(l.home, p); err == nil && !strings.HasPrefix(r, "..") {
			return "~/" + r
		}
	}
	return p
}

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

var importRE = regexp.MustCompile(`(?:^|\s)@(\S+)`)

// importPaths lists the @path tokens of a rules file, in order, outside
// fenced blocks and code spans.
func importPaths(text string) []string {
	var out []string
	fence := "" // the opening run of the fenced block we are in
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if fence != "" {
			if run := fenceRun(t); run != "" && run[0] == fence[0] && len(run) >= len(fence) && strings.TrimSpace(t[len(run):]) == "" {
				fence = ""
			}
			continue
		}
		if run := fenceRun(t); run != "" {
			fence = run
			continue
		}
		for _, m := range importRE.FindAllStringSubmatch(stripCodeSpans(line), -1) {
			p := strings.TrimRight(m[1], ".,;:!?)]}\"'")
			if strings.ContainsAny(p, "/.") && !strings.HasPrefix(p, "@") {
				out = append(out, p)
			}
		}
	}
	return out
}

// fenceRun is the ``` or ~~~ run (3 or more) a line starts with, or "".
func fenceRun(t string) string {
	if len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return t[:n]
}

// stripCodeSpans blanks `code spans` on one line. An unclosed run of
// backticks is kept as text, as Markdown does.
func stripCodeSpans(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		run := s[i:j]
		k := strings.Index(s[j:], run)
		if k < 0 {
			b.WriteString(run)
			i = j
			continue
		}
		b.WriteByte(' ')
		i = j + k + len(run)
	}
	return b.String()
}

// rootNames is the root files as the heading names them.
func (s rulesSet) rootNames() string {
	var n []string
	for _, d := range s.Roots {
		n = append(n, d.Name)
		if d.Also != "" {
			n = append(n, d.Also)
		}
	}
	return strings.Join(n, ", ")
}

// importsHash keys a summary: it is valid while the imports read the same.
func (s rulesSet) importsHash() string {
	h := sha256.New()
	for _, d := range s.Imports {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", d.Name, d.Note, d.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// importedText is the imports that were read, in full.
func (s rulesSet) importedText() bool {
	for _, d := range s.Imports {
		if d.Text != "" {
			return true
		}
	}
	return false
}

func (d rulesDoc) heading() string {
	h := "\n\n### " + d.Name
	if d.Also != "" {
		h += " (also " + d.Also + ")"
	}
	if d.From != "" {
		h += " (imported by " + d.From + ")"
	}
	return h
}

// noteLine is a doc that has no text: one line, not a section.
func (d rulesDoc) noteLine() string {
	return "\n\n(" + d.Name + ", imported by " + d.From + ": " + d.Note + ")"
}

// files is each context file's share of the block in mode, estimated: the
// root files and, in full mode, each import; in summary or index mode the
// imports are one row (the list, with the condensed text or the pointer to
// the index). Nil when there is no root file.
func (s rulesSet) files(mode string) []ContextPart {
	if len(s.Roots) == 0 {
		return nil
	}
	var out []ContextPart
	for _, d := range s.Roots {
		out = append(out, ContextPart{Name: d.Name, Tokens: estimateTokens(d.heading() + "\n\n" + d.Text)})
	}
	if len(s.Imports) == 0 {
		return out
	}
	switch mode {
	case RulesSummary, RulesIndex:
		what := "imports, condensed"
		if mode == RulesIndex {
			what = "imports, in the index"
		}
		n := 0
		for _, d := range s.Imports {
			n += estimateTokens("\n- " + d.Name)
		}
		out = append(out, ContextPart{Name: fmt.Sprintf("%d %s", len(s.Imports), what), Tokens: n})
	default:
		for _, d := range s.Imports {
			text := d.noteLine()
			if d.Text != "" {
				text = d.heading() + "\n\n" + d.Text
			}
			out = append(out, ContextPart{Name: d.Name, Tokens: estimateTokens(text)})
		}
	}
	return out
}

// render is the system-prompt block in mode: full, summary (with the cached
// summary text) or index. "" when there is no root file.
func (s rulesSet) render(mode, summary string) string {
	if len(s.Roots) == 0 {
		return s.offSection() // every root switched off: only their names
	}
	var b strings.Builder
	b.WriteString("\n\n## Project rules (" + s.rootNames() + ")\n\n" +
		"These rules apply to this workspace. Nested directories may have their " +
		"own AGENTS.md; the code tool will ask you to read one before you touch " +
		"files under it.")
	for _, d := range s.Roots {
		b.WriteString(d.heading() + "\n\n" + d.Text)
		if d.Note != "" {
			b.WriteString("\n\n(" + d.Note + ")")
		}
	}
	if len(s.Imports) == 0 {
		return b.String()
	}
	list := func() {
		for _, d := range s.Imports {
			b.WriteString("\n- " + d.Name)
			if d.Text == "" {
				b.WriteString(" (" + d.Note + ")")
			}
		}
	}
	switch mode {
	case RulesSummary:
		b.WriteString("\n\n### Imported files, condensed\n\nThe rules above import these files:")
		list()
		b.WriteString("\n\nWhat follows is a condensed version of them, made once by /rules summary. " +
			"Read a file itself when you need its exact words.\n\n" + strings.TrimSpace(summary))
	case RulesIndex:
		b.WriteString("\n\n### Imported files (searchable)\n\nThe rules above import these files. " +
			"They are not copied here: they are in the docs index, so the search tool finds them, " +
			"and a part that matches what you are doing is surfaced to you. Read a file itself when you need it whole.")
		list()
	default:
		for _, d := range s.Imports {
			if d.Text == "" {
				b.WriteString(d.noteLine())
				continue
			}
			b.WriteString(d.heading() + "\n\n" + d.Text)
			if d.Note != "" {
				b.WriteString("\n\n(" + d.Note + ")")
			}
		}
	}
	b.WriteString(s.offSection())
	return b.String()
}

// offSection names the files switched off, "" when none is.
func (s rulesSet) offSection() string {
	if len(s.Off) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n### Rule files kept out of these instructions\n\nThe user switched these files off " +
		"(/context-file): they are not copied here. They are in the docs index when it runs, so a part that " +
		"matches what you are doing is surfaced to you; read one when you need it.")
	for _, d := range s.Off {
		b.WriteString("\n- " + d.Name)
	}
	return b.String()
}

// rulesState is what dun keeps about a project's rules, in
// $DUN_HOME/projects/<id>/rules.json.
type rulesState struct {
	Mode    string        `json:"mode,omitempty"` // "" is full
	Summary *rulesSummary `json:"summary,omitempty"`
	// Offered is the hash of the full block the user was last told about,
	// so the offer is made once per content change.
	Offered string `json:"offered,omitempty"`
	// Indexed is what is in the rules index: path → size and mtime.
	Indexed map[string]string `json:"indexed,omitempty"`
}

type rulesSummary struct {
	Hash string `json:"hash"` // rulesSet.importsHash of the sources
	Text string `json:"text"`
}

// rulesStatePath is the state file of the project the session is in, or ""
// when there is no project dir.
func (h *Harness) rulesStatePath() string {
	p, err := EnsureProject(h.configDir())
	if err != nil {
		return ""
	}
	return filepath.Join(p.StateDir, rulesStateFile)
}

func (h *Harness) readRulesState() rulesState {
	var st rulesState
	if p := h.rulesStatePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(b, &st); err != nil {
				log.Printf("dun: %s: %v", p, err)
			}
		}
	}
	return st
}

func (h *Harness) writeRulesState(st rulesState) error {
	p := h.rulesStatePath()
	if p == "" {
		return fmt.Errorf("no project state dir")
	}
	return writeFileAtomic(p, mustJSON(st), 0o600)
}

// updateRulesState applies f to the saved state under the lock.
func (h *Harness) updateRulesState(f func(*rulesState)) error {
	h.rulesMu.Lock()
	defer h.rulesMu.Unlock()
	st := h.readRulesState()
	f(&st)
	return h.writeRulesState(st)
}

// effectiveMode is the mode the block is rendered in: the saved one when it
// can be honoured, else full.
func effectiveMode(st rulesState, set rulesSet, docsOn bool) string {
	switch st.Mode {
	case RulesSummary:
		if st.Summary != nil && st.Summary.Hash == set.importsHash() {
			return RulesSummary
		}
	case RulesIndex:
		if docsOn {
			return RulesIndex
		}
	}
	return RulesFull
}

// rulesBlock is the rules block for the system prompt, the mode it is in
// ("" when there are no rules) and the files it was made from. docsOn says
// the docs server runs (index mode needs it). On a root session it also
// makes the size offer.
func (h *Harness) rulesBlock(docsOn bool) (string, string, rulesSet) {
	set := h.switchRules(loadRules(h.cfg.Workspace))
	if len(set.Roots) == 0 && len(set.Off) == 0 {
		return "", "", set
	}
	h.rulesMu.Lock()
	st := h.readRulesState()
	h.rulesMu.Unlock()
	st.Mode = h.contextFileMode(st.Mode)
	mode := effectiveMode(st, set, docsOn)
	summary := ""
	if mode == RulesSummary {
		summary = st.Summary.Text
	}
	block := set.render(mode, summary)
	if h.isChild() {
		return block, mode, set
	}
	// The offer: only in full mode (a summary or an index is the answer to
	// it), only when there are imports to make cheaper, and once per content.
	if mode == RulesFull && set.importedText() && h.cfg.OnRules != nil {
		if n := h.meter.Estimate(block); n > rulesOfferTokens {
			sum := sha256.Sum256([]byte(block))
			hash := hex.EncodeToString(sum[:])
			if st.Offered != hash {
				_ = h.updateRulesState(func(s *rulesState) { s.Offered = hash })
				h.cfg.OnRules(fmt.Sprintf("project rules: %s with %d imported file(s) cost ~%s tokens on every request. "+
					"/rules summary condenses the imports, /rules index makes them searchable instead; /context-file switches one file out of every request and shows what each costs.",
					set.rootNames(), len(set.Imports), Grouped(n)))
			}
		}
	}
	return block, mode, set
}

// rulesIndexWant is what the rules index should hold: the imports that were
// read, in index mode, else nothing.
func rulesIndexWant(set rulesSet, mode string) map[string]string {
	want := map[string]string{}
	// A file switched off is indexed whatever the mode: that is how it is
	// still found.
	for _, d := range set.Off {
		if st, err := os.Stat(d.Path); err == nil && d.Text != "" {
			want[d.Path] = fmt.Sprintf("%d %d", st.Size(), st.ModTime().UnixNano())
		}
	}
	if mode != RulesIndex {
		return want
	}
	for _, d := range set.Imports {
		if d.Text == "" {
			continue
		}
		if st, err := os.Stat(d.Path); err == nil {
			want[d.Path] = fmt.Sprintf("%d %d", st.Size(), st.ModTime().UnixNano())
		}
	}
	return want
}

// syncRulesIndex brings the rules index in step with the mode: in index mode
// the imports are ingested (again when changed), otherwise whatever an
// earlier session put there is forgotten. What is indexed is saved, so a
// later process can forget it.
func (h *Harness) syncRulesIndex(sink *mcpDocsSink, set rulesSet, mode string) {
	if sink == nil {
		return
	}
	h.rulesIdxMu.Lock()
	defer h.rulesIdxMu.Unlock()
	h.rulesMu.Lock()
	have := h.readRulesState().Indexed
	h.rulesMu.Unlock()
	want := rulesIndexWant(set, mode)
	now, err := syncIndex(sink, rulesIndex, have, want)
	if err != nil {
		log.Printf("dun: rules index: %v", err)
	}
	if !maps.Equal(have, now) {
		if len(now) == 0 {
			now = nil
		}
		if err := h.updateRulesState(func(s *rulesState) { s.Indexed = now }); err != nil {
			log.Printf("dun: rules index: %v", err)
		}
	}
	h.rulesDocsMu.Lock()
	h.rulesDocs = map[string]bool{}
	for p := range now {
		h.rulesDocs[p] = true
	}
	h.rulesDocsMu.Unlock()
}

// isRulesDoc says a surfaced document is an imported rules file.
func (h *Harness) isRulesDoc(docID string) bool {
	h.rulesDocsMu.Lock()
	defer h.rulesDocsMu.Unlock()
	return h.rulesDocs[docID]
}

// syncIndex makes index hold want (path → signature) given that it holds
// have: what is gone is forgotten, what is new or changed is ingested. It
// returns what the index holds afterwards, which on an error is have with the
// steps that succeeded applied.
func syncIndex(sink *mcpDocsSink, index string, have, want map[string]string) (map[string]string, error) {
	now := map[string]string{}
	for p, s := range have {
		now[p] = s
	}
	var gone, changed []string
	for p := range have {
		if _, ok := want[p]; !ok {
			gone = append(gone, p)
		}
	}
	for p, s := range want {
		if have[p] != s {
			changed = append(changed, p)
		}
	}
	sort.Strings(gone)
	sort.Strings(changed)
	if len(gone) > 0 {
		if err := sink.forget(index, gone); err != nil {
			return now, err
		}
		for _, p := range gone {
			delete(now, p)
		}
	}
	if len(changed) > 0 {
		if err := sink.ingest(index, changed); err != nil {
			return now, err
		}
		for _, p := range changed {
			now[p] = want[p]
		}
	}
	return now, nil
}

// RulesStatus is bare /rules: the files, their sizes and cost, and the mode.
func (h *Harness) RulesStatus() string {
	set := loadRules(h.Workspace())
	if len(set.Roots) == 0 {
		return "rules: no AGENTS.md or CLAUDE.md at the workspace root"
	}
	h.rulesMu.Lock()
	st := h.readRulesState()
	h.rulesMu.Unlock()
	docsOn := h.hookStarted(HookRulesChanged)
	mode := effectiveMode(st, set, docsOn)
	saved := firstNonEmptyStr(st.Mode, RulesFull)
	var b strings.Builder
	fmt.Fprintf(&b, "rules: mode %s", mode)
	if saved != mode {
		why := "the summary is out of date: the files changed (/rules summary makes a new one)"
		if saved == RulesSummary && st.Summary == nil {
			why = "no summary yet"
		}
		if saved == RulesIndex {
			why = "the docs index is off (/raglit on)"
		}
		fmt.Fprintf(&b, " (saved: %s, but %s)", saved, why)
	}
	row := func(d rulesDoc) {
		fmt.Fprintf(&b, "\n  %-28s ", d.Name)
		if d.Text == "" && d.From != "" {
			b.WriteString(d.Note)
			return
		}
		fmt.Fprintf(&b, "%7s  ~%s tokens", HumanBytes(int64(d.Size)), Grouped(h.meter.Estimate(d.Text)))
		if d.Also != "" {
			b.WriteString("  (also " + d.Also + ")")
		}
		if d.From != "" {
			b.WriteString("  imported by " + d.From)
		}
		if d.Note != "" {
			b.WriteString("  " + d.Note)
		}
	}
	for _, d := range set.Roots {
		row(d)
	}
	for _, d := range set.Imports {
		row(d)
	}
	full := h.meter.Estimate(set.render(RulesFull, ""))
	fmt.Fprintf(&b, "\n  block: ~%s tokens in mode full", Grouped(full))
	if mode != RulesFull {
		sum := ""
		if st.Summary != nil {
			sum = st.Summary.Text
		}
		fmt.Fprintf(&b, ", ~%s in mode %s", Grouped(h.meter.Estimate(set.render(mode, sum))), mode)
	} else if st.Summary != nil && st.Summary.Hash == set.importsHash() {
		fmt.Fprintf(&b, ", ~%s with the saved summary", Grouped(h.meter.Estimate(set.render(RulesSummary, st.Summary.Text))))
	}
	b.WriteString(" (estimated)")
	if len(set.Imports) == 0 {
		b.WriteString("\n  no imports: summary and index change nothing (they act on imported files only)")
	}
	b.WriteString("\n  /rules full · /rules summary · /rules index")
	return b.String()
}

// SetRulesMode carries out /rules full|summary|index and returns the line
// for the user. summary asks the model, so it can take a while.
func (h *Harness) SetRulesMode(ctx context.Context, mode string) (string, error) {
	set := loadRules(h.Workspace())
	switch mode {
	case RulesFull:
	case RulesIndex:
		if len(set.Imports) == 0 {
			return "", fmt.Errorf("the root rules import no files: there is nothing to index")
		}
		if !h.hookStarted(HookRulesChanged) {
			return "", fmt.Errorf("index mode needs the docs index: /raglit on first")
		}
	case RulesSummary:
		if err := h.ensureRulesSummary(ctx, set); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("usage: /rules [full|summary|index]")
	}
	if err := h.updateRulesState(func(s *rulesState) {
		s.Mode = mode
		if mode == RulesFull {
			s.Mode = ""
		}
	}); err != nil {
		return "", err
	}
	h.applyTools()
	st := h.readRulesState()
	sum := ""
	if st.Summary != nil {
		sum = st.Summary.Text
	}
	full := h.meter.Estimate(set.render(RulesFull, ""))
	msg := fmt.Sprintf("rules: mode %s · ~%s tokens", mode, Grouped(full))
	if mode != RulesFull {
		msg = fmt.Sprintf("rules: mode %s · ~%s tokens, was ~%s in mode full", mode, Grouped(h.meter.Estimate(set.render(mode, sum))), Grouped(full))
	}
	if mode == RulesIndex {
		msg += fmt.Sprintf(" · %d file(s) going into index %q", len(rulesIndexWant(set, RulesIndex)), rulesIndex)
	}
	return msg + " (estimated)", nil
}

// rulesSummaryPrompt asks for a condensed version of the imported files.
const rulesSummaryPrompt = "Below are files of a software project's rules for a coding agent. " +
	"Write a condensed version of them that loses NO rule. Keep every instruction, constraint, " +
	"prohibition, command, path, name, number and identifier exactly as written; drop examples, " +
	"repetition, history and explanation of why. Do not add anything that is not in the files. " +
	"Keep one short heading per file, naming its path. Reply with only the condensed text.\n\n"

// summarizeRules asks the model, once, for a condensed version of the imports.
func (h *Harness) summarizeRules(ctx context.Context, set rulesSet) (string, error) {
	if h.Session == nil || h.Session.Runner == nil {
		return "", fmt.Errorf("no model to ask")
	}
	var b strings.Builder
	b.WriteString(rulesSummaryPrompt)
	for _, d := range set.Imports {
		if d.Text != "" {
			b.WriteString("=== " + d.Name + " ===\n" + d.Text + "\n\n")
		}
	}
	start := time.Now()
	ch, err := untee(h.Session.Runner).ChatStream(ctx, []llm.Message{{Role: "user", Content: b.String()}}, nil, nil)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("rules", start, nil)
			return "", fmt.Errorf("%s", c.Error)
		}
		out.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("rules", start, usage)
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", fmt.Errorf("the model returned no summary")
	}
	return text, nil
}

// ensureRulesSummary writes the imports' summary when there is none for
// their current content. It does not change the mode.
func (h *Harness) ensureRulesSummary(ctx context.Context, set rulesSet) error {
	if !set.importedText() {
		return fmt.Errorf("the root rules import no files: there is nothing to condense")
	}
	h.rulesMu.Lock()
	st := h.readRulesState()
	h.rulesMu.Unlock()
	if st.Summary != nil && st.Summary.Hash == set.importsHash() {
		return nil
	}
	text, err := h.summarizeRules(ctx, set)
	if err != nil {
		return err
	}
	full := h.meter.Estimate(set.render(RulesFull, ""))
	if cond := h.meter.Estimate(set.render(RulesSummary, text)); cond >= full {
		return fmt.Errorf("the summary (~%s tokens) is not smaller than the files (~%s): kept mode full", Grouped(cond), Grouped(full))
	}
	hash := set.importsHash()
	return h.updateRulesState(func(s *rulesState) { s.Summary = &rulesSummary{Hash: hash, Text: text} })
}
