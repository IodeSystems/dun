package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Skills — instructions for one kind of task, loaded when the task comes up.
//
// The format is Agent Skills, the one Claude Code reads: a directory holding
// SKILL.md, whose YAML frontmatter has a name and a description, whose body
// is the instructions, and which may mention other files in the directory.
// Only the name and description ride every request (in the skill tool's
// description);
// the body costs tokens only once the model calls skill for it. That split is
// the point — a skill library can be large without the prompt being large.
//
// Sources, lowest first; a later one wins on a name it shares with an earlier:
//
//	~/.claude/skills/*/SKILL.md      the user's Claude Code skills
//	$DUN_HOME/skills/*/SKILL.md      the user's dun skills
//	MCP servers                      a resource skill://… or …/SKILL.md
//	<project>/.claude/skills/…       the project's, Claude Code layout
//	<project>/.dun/skills/…          the project's, dun layout
//
// A skill whose frontmatter says disable-model-invocation: true is the user's
// to invoke (/skill <name>) and is not offered to the model.

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is where it came from: "user", "project" or "mcp:<server>".
	Source string `json:"source"`
	// Dir is a local skill's directory. Server and URI locate an MCP one;
	// Files are the other resources under the same prefix.
	Dir      string   `json:"-"`
	Server   string   `json:"-"`
	URI      string   `json:"-"`
	Files    []string `json:"-"`
	UserOnly bool     `json:"userOnly,omitempty"`
}

const (
	skillDescMax   = 300 // a description's share of the system prompt
	skillListMax   = 80  // skills listed in the system prompt
	skillBodyMax   = 64 << 10
	skillResPrefix = "skill://"
)

// userHomeDir finds ~/.claude/skills; a variable so tests can keep a user's
// own skills out of them.
var userHomeDir = os.UserHomeDir

// skillDirs lists the local skill roots, lowest precedence first.
func skillDirs(projectDir string) []struct{ dir, source string } {
	home, _ := userHomeDir()
	out := []struct{ dir, source string }{}
	if home != "" {
		out = append(out, struct{ dir, source string }{filepath.Join(home, ".claude", "skills"), "user"})
	}
	out = append(out, struct{ dir, source string }{filepath.Join(filepath.Dir(SessionsDir()), "skills"), "user"})
	if projectDir != "" {
		out = append(out,
			struct{ dir, source string }{filepath.Join(projectDir, ".claude", "skills"), "project"},
			struct{ dir, source string }{filepath.Join(projectDir, DunDir, "skills"), "project"})
	}
	return out
}

// withSynced adds, after each root, the buckets Claude syncs skills into:
// <root>/synced/<bucket>/<name>/SKILL.md (claude.ai's skills, USER
// 2026-10-22). A skill directly in the root wins over a synced one of the
// same name.
func withSynced(roots []struct{ dir, source string }) []struct{ dir, source string } {
	var out []struct{ dir, source string }
	for _, r := range roots {
		buckets, _ := os.ReadDir(filepath.Join(r.dir, "synced"))
		for _, b := range buckets {
			d := filepath.Join(r.dir, "synced", b.Name())
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				out = append(out, struct{ dir, source string }{d, r.source})
			}
		}
		out = append(out, r)
	}
	return out
}

// loadLocalSkills reads every */SKILL.md under the roots, and under their
// synced buckets.
func loadLocalSkills(roots []struct{ dir, source string }) []Skill {
	var out []Skill
	for _, r := range withSynced(roots) {
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(r.dir, e.Name())
			if st, err := os.Stat(dir); err != nil || !st.IsDir() { // follows symlinks
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				continue
			}
			if s, ok := parseSkill(string(b), e.Name()); ok {
				s.Source, s.Dir = r.source, dir
				out = append(out, s)
			}
		}
	}
	return out
}

// parseSkill reads a SKILL.md's frontmatter. fallback names a skill whose
// frontmatter has no name. A skill with no description is not usable — the
// model could not tell when to load it — and is dropped.
func parseSkill(text, fallback string) (Skill, bool) {
	meta, _ := splitFrontmatter(text)
	s := Skill{Name: firstNonEmptyStr(meta["name"], fallback), Description: meta["description"]}
	s.UserOnly = strings.EqualFold(meta["disable-model-invocation"], "true")
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.Join(strings.Fields(s.Description), " ")
	return s, s.Name != "" && s.Description != ""
}

// splitFrontmatter returns the top-level scalar keys of a leading ---
// block, and the body after it. It reads the subset SKILL.md files use:
// key: value, quoted values, and | or > blocks for a long description.
func splitFrontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return meta, text
	}
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, text
	}
	block, body := rest[:end], rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "|" || v == ">" || v == "|-" || v == ">-" {
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			v = strings.TrimSpace(strings.Join(parts, " "))
		} else if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			if v[0] == '"' {
				var u string
				if json.Unmarshal([]byte(v), &u) == nil {
					v = u
				} else {
					v = v[1 : len(v)-1]
				}
			} else {
				v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
			}
		}
		meta[k] = v
	}
	return meta, body
}

// isSkillResource reports whether an MCP resource is a skill's SKILL.md.
func isSkillResource(r mcpmgr.Resource) bool {
	return path.Base(r.URI) == "SKILL.md" && (strings.HasPrefix(r.URI, skillResPrefix) || strings.Contains(r.URI, "/skills/"))
}

// uriDir is a URI up to its last slash. Not path.Dir: that would clean
// "skill://x" to "skill:/x".
func uriDir(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[:i]
	}
	return u
}

// isSkillFile reports whether a resource belongs to a skill's directory: a
// skill:// resource, or any file next to a SKILL.md — these are not docs.
func isSkillFile(r mcpmgr.Resource) bool {
	return strings.HasPrefix(r.URI, skillResPrefix) || isSkillResource(r)
}

// discoverMCPSkills reads the SKILL.md resources among rs.
func discoverMCPSkills(ctx context.Context, rr resourceReader, id string, rs []mcpmgr.Resource) []Skill {
	var out []Skill
	for _, r := range rs {
		if !isSkillResource(r) {
			continue
		}
		cs, err := rr.ReadResource(ctx, id, r.URI)
		if err != nil || len(cs) == 0 {
			continue
		}
		dir := uriDir(r.URI)
		s, ok := parseSkill(cs[0].Text, path.Base(dir))
		if !ok {
			continue
		}
		s.Source, s.Server, s.URI = "mcp:"+id, id, r.URI
		for _, o := range rs {
			if o.URI != r.URI && strings.HasPrefix(o.URI, dir+"/") {
				s.Files = append(s.Files, strings.TrimPrefix(o.URI, dir+"/"))
			}
		}
		out = append(out, s)
	}
	return out
}

// mcpCatalog is what a started server offers besides tools.
type mcpCatalog struct {
	skills  []Skill
	prompts []mcpmgr.Prompt
}

// discoverCatalog lists a just-started server's skills and prompts. A server
// that declares neither capability is not asked.
func (h *Harness) discoverCatalog(ctx context.Context, id string) {
	caps := h.mgr.Capabilities(id)
	var c mcpCatalog
	if _, ok := caps["resources"]; ok {
		if rs, err := h.mgr.ListResources(ctx, id); err == nil {
			c.skills = discoverMCPSkills(ctx, h.mgr, id, rs)
		}
	}
	if _, ok := caps["prompts"]; ok {
		c.prompts, _ = h.mgr.ListPrompts(ctx, id)
	}
	h.srvMu.Lock()
	if h.catalog == nil {
		h.catalog = map[string]mcpCatalog{}
	}
	h.catalog[id] = c
	h.srvMu.Unlock()
}

// Skills returns every skill in reach, by precedence: a later source
// replaces an earlier one of the same name. Sorted by name.
func (h *Harness) Skills() []Skill {
	user, project := h.skillRoots()
	all := loadLocalSkills(user)
	h.srvMu.Lock()
	ids := make([]string, 0, len(h.catalog))
	for id := range h.catalog {
		if h.mgr.ServerStarted(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		all = append(all, h.catalog[id].skills...)
	}
	h.srvMu.Unlock()
	all = append(all, loadLocalSkills(project)...)

	by := map[string]Skill{}
	for _, s := range all {
		by[s.Name] = s
	}
	out := make([]Skill, 0, len(by))
	for _, s := range by {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// skillRoots is the local skill roots: the user's (below the servers'
// skills in precedence) and the project's (above them).
func (h *Harness) skillRoots() (user, project []struct{ dir, source string }) {
	dir := h.cfg.ConfigDir
	if dir == "" {
		dir = h.cfg.Workspace
	}
	roots := skillDirs(dir)
	if dir == "" {
		return roots, nil
	}
	return roots[:len(roots)-2], roots[len(roots)-2:]
}

// skillsSig fingerprints the local skill roots: every skill directory's
// SKILL.md path, size and modification time. Cheap enough to poll.
func skillsSig(roots []struct{ dir, source string }) string {
	var b strings.Builder
	for _, r := range withSynced(roots) {
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(r.dir, e.Name(), "SKILL.md")
			if st, err := os.Stat(p); err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", p, st.Size(), st.ModTime().UnixNano())
			}
		}
	}
	return b.String()
}

// CheckSkills reports whether a local skill was added, removed or edited
// since the last check. If so the tool set is rebuilt — the skill tool's
// description lists the skills, so the model would not otherwise see a new
// one — at the end of the running turn if there is one, and the model is told
// which skills came and went. The first call only records.
func (h *Harness) CheckSkills() bool {
	user, project := h.skillRoots()
	sig := skillsSig(append(append([]struct{ dir, source string }{}, user...), project...))
	h.skillMu.Lock()
	first, changed := !h.skillSeen, sig != h.skillSig
	h.skillSeen, h.skillSig = true, sig
	before := h.skillNames
	var now []string
	for _, s := range h.Skills() {
		now = append(now, s.Name)
	}
	h.skillNames = now
	h.skillMu.Unlock()
	if first || !changed {
		return false
	}
	var added, gone []string
	for _, n := range now {
		if !slices.Contains(before, n) {
			added = append(added, n)
		}
	}
	for _, n := range before {
		if !slices.Contains(now, n) {
			gone = append(gone, n)
		}
	}
	switch {
	case len(added) > 0 || len(gone) > 0:
		note := "skills changed:"
		if len(added) > 0 {
			note += " new " + strings.Join(added, ", ") + ";"
		}
		if len(gone) > 0 {
			note += " gone " + strings.Join(gone, ", ") + ";"
		}
		h.Aside(strings.TrimSuffix(note, ";") + " (the skill tool lists them)")
	default:
		h.Aside("a skill was edited; load it again with the skill tool for the new text")
	}
	h.applyTools()
	return true
}

// skillsIndex is the raglit index local skills go into, in the docs server's
// store: the proactive search then surfaces a skill that matches the
// conversation, without the model knowing to ask for it (USER 2026-10-22).
const skillsIndex = "skills"

// A skill's reference files go into the index too (USER 2026-10-22): a
// match is often in the file SKILL.md points at, not in SKILL.md. Only text
// documents, small ones, a bounded number per skill; scripts/ and hidden
// directories are code and tooling, not instructions.
const (
	skillRefMax       = 64 << 10 // a reference file larger than this is not indexed
	skillRefsPerSkill = 50
)

var skillRefExt = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".rst": true}

// skillRefs lists a local skill's reference files, relative to its dir.
func skillRefs(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(out) >= skillRefsPerSkill {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || name == "scripts" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !skillRefExt[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "SKILL.md" {
			return nil
		}
		if st, err := d.Info(); err != nil || st.Size() > skillRefMax {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out
}

// skillHit is an indexed file of a skill: its SKILL.md (File "") or a
// reference file (File relative to the skill's dir).
type skillHit struct {
	Skill
	File string
}

// syncSkillIndex brings the skills index in step with the local skills the
// model may use: a SKILL.md or reference file new or changed since the last
// sync is ingested, one that is gone is forgotten. Called whenever the tool
// set is rebuilt with the docs server running — that is, when /rag starts
// and when a skill changes. MCP servers' skills stay in their servers.
func (h *Harness) syncSkillIndex(sink *mcpDocsSink) {
	if sink == nil {
		return
	}
	h.skillIdxMu.Lock()
	defer h.skillIdxMu.Unlock()
	cur := map[string]string{}
	docs := map[string]skillHit{}
	add := func(s Skill, rel string) {
		p := filepath.Join(s.Dir, rel)
		if st, err := os.Stat(p); err == nil {
			cur[p] = fmt.Sprintf("%d %d", st.Size(), st.ModTime().UnixNano())
			hit := skillHit{Skill: s}
			if rel != "SKILL.md" {
				hit.File = rel
			}
			docs[p] = hit
		}
	}
	for _, s := range h.Skills() {
		if s.Dir == "" || s.UserOnly {
			continue
		}
		add(s, "SKILL.md")
		for _, rel := range skillRefs(s.Dir) {
			add(s, rel)
		}
	}
	h.skillDocs = docs
	now, err := syncIndex(sink, skillsIndex, h.skillIdx, cur)
	if err != nil {
		log.Printf("dun: skills index: %v", err)
	}
	h.skillIdx = now
}

// skillForDoc names the skill a surfaced document belongs to, so the
// surfacing can point at the skill instead of quoting a fragment of it.
func (h *Harness) skillForDoc(docID string) (skillHit, bool) {
	h.skillIdxMu.Lock()
	defer h.skillIdxMu.Unlock()
	s, ok := h.skillDocs[docID]
	return s, ok
}

// PromptInfo is one MCP prompt, for a UI to offer as a slash command.
type PromptInfo struct {
	Server      string                  `json:"server"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Arguments   []mcpmgr.PromptArgument `json:"arguments,omitempty"`
}

// Prompts returns the prompts of every running server, by server then name.
func (h *Harness) Prompts() []PromptInfo {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	var out []PromptInfo
	for id, c := range h.catalog {
		if !h.mgr.ServerStarted(id) {
			continue
		}
		for _, p := range c.prompts {
			out = append(out, PromptInfo{Server: id, Name: p.Name, Description: firstNonEmptyStr(p.Description, p.Title), Arguments: p.Arguments})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RenderPrompt gets one MCP prompt with its arguments and renders its
// messages as the text of one user message.
func (h *Harness) RenderPrompt(ctx context.Context, server, name string, args map[string]string) (string, error) {
	_, msgs, err := h.mgr.GetPrompt(ctx, server, name, args)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, m := range msgs {
		if t := strings.TrimSpace(m.Text()); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("prompt %s returned no text", name)
	}
	return strings.Join(parts, "\n\n"), nil
}

// skillList renders the model's skills as "- name: description" lines, or
// "" when there are none it may use.
func skillList(skills []Skill) string {
	var b strings.Builder
	n := 0
	for _, s := range skills {
		if s.UserOnly {
			continue
		}
		if n == skillListMax {
			fmt.Fprintf(&b, "\n- … and more; call skill with a name you expect to exist")
			break
		}
		d := s.Description
		if len(d) > skillDescMax {
			d = d[:skillDescMax] + "…"
		}
		fmt.Fprintf(&b, "\n- %s: %s", s.Name, d)
		n++
	}
	return b.String()
}

// skillToolDef carries the skill list in its own description. The list
// was in the system prompt at first; live, the model matched a task to the
// tools it had and never to a skill listed there (plan item 12).
func skillToolDef(list string) llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "skill"
	td.Function.Description = "Load a skill: instructions for one kind of task, which tell you how this user " +
		"wants it done. When the task in front of you matches a skill below, call this FIRST — before any " +
		"other tool — and follow what it says. Pass file to read a file the skill mentions. Skills:" + list
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "the skill's name"},
			"file": map[string]any{"type": "string", "description": "optional: a file the skill mentions, relative to the skill"},
		},
		"required": []string{"name"},
	}
	return td
}

// loadSkill renders the skill tool's result: the SKILL.md body, or one of
// the skill's files.
func (h *Harness) loadSkill(ctx context.Context, name, file string) string {
	var s *Skill
	for _, k := range h.Skills() {
		if k.Name == name {
			s = &k
			break
		}
	}
	if s == nil {
		var names []string
		for _, k := range h.Skills() {
			names = append(names, k.Name)
		}
		return fmt.Sprintf("ERROR: no skill %q (have: %s)", name, strings.Join(names, ", "))
	}
	if s.Server != "" {
		uri := s.URI
		if file != "" {
			uri = uriDir(s.URI) + "/" + strings.TrimPrefix(path.Clean("/"+file), "/")
		}
		cs, err := h.mgr.ReadResource(ctx, s.Server, uri)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		var text strings.Builder
		for _, c := range cs {
			text.WriteString(c.Text)
		}
		out := text.String()
		if file == "" {
			_, out = splitFrontmatter(out)
			if len(s.Files) > 0 {
				out += "\n\n(Other files of this skill, read with skill name=" + s.Name + " file=<path>: " + strings.Join(s.Files, ", ") + ")"
			}
		}
		return clipSkill(out)
	}
	if file != "" {
		p := filepath.Join(s.Dir, filepath.Clean("/"+file))
		b, err := os.ReadFile(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return clipSkill(string(b))
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "SKILL.md"))
	if err != nil {
		return "ERROR: " + err.Error()
	}
	_, body := splitFrontmatter(string(b))
	return clipSkill(body) + "\n\n(This skill's directory is " + s.Dir + "; paths it mentions are relative to it. " +
		"Read one with skill name=" + s.Name + " file=<path>, or run its scripts from there.)"
}

func clipSkill(s string) string {
	if len(s) > skillBodyMax {
		return s[:skillBodyMax] + "\n… (cut at " + fmt.Sprint(skillBodyMax) + " bytes)"
	}
	return s
}

// withSkill handles the skill tool locally; everything else routes onward.
func withSkill(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "skill" {
			return inner(ctx, tc)
		}
		var a struct {
			Name string `json:"name"`
			File string `json:"file"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
		out := h.loadSkill(ctx, strings.TrimSpace(a.Name), strings.TrimSpace(a.File))
		if onCall != nil {
			onCall("skill", map[string]any{"name": a.Name, "file": a.File}, out)
		}
		return out, nil
	}
}
