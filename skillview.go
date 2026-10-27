package dun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Reading a skill as a person does (the /skill browser): its files' text and
// the references in them. Display only: the model's view of a skill (the
// skill tool, loadSkill) does not change. A reference is one of two kinds,
// and neither is new syntax for a skill's author:
//
//   - a link: a markdown link or a plain path to a file in the skill's
//     folder ("see references/forms.md", [forms](forms.md), `x/y.md`);
//   - an include: an @path, as in Claude Code's CLAUDE.md imports, which a
//     reader shows inlined (at most SkillIncludeDepth deep, each file once).

// SkillRef is one reference in a skill file's text.
type SkillRef struct {
	Include    bool   // an @path: shown inlined
	Start, End int    // the reference's byte span in the text
	Target     string // the path as written, any #fragment cut
	Text       string // what a reader shows: a markdown link's text, else the path
	// Bare: a plain path with no "/" (README.md). It is a link only when
	// the file exists: prose has words with dots in them (e.g., node.js).
	Bare bool
}

const (
	SkillIncludeDepth = 3
	skillViewMax      = 256 << 10 // a file larger than this is cut
	skillIncludesMax  = 1 << 20   // the included files' text, all together
	skillViewFilesMax = 1000
)

var (
	mdLinkRe   = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	pathTokRe  = regexp.MustCompile(`@?[A-Za-z0-9_.][A-Za-z0-9_./\-]*`)
	pathExtRe  = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]{0,7}$`)
	tokenStart = " \t([\"'<*_,"
)

// SkillRefs finds the references in a markdown text, in order. In code (a
// fenced block or a `span`) a plain path is still a link; markdown links and
// includes are not read there (as Claude Code does not import from code).
func SkillRefs(text string) []SkillRef {
	var out []SkillRef
	fence := ""
	at := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		base := at
		at += len(line)
		trim := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trim, fence) {
				fence = ""
				continue
			}
			out = append(out, plainRefs(line, base, true)...)
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence = trim[:3]
			continue
		}
		// Split at backticks: odd segments are code spans.
		seg, inCode := 0, false
		for i := 0; i <= len(line); i++ {
			if i < len(line) && line[i] != '`' {
				continue
			}
			part := line[seg:i]
			if inCode {
				out = append(out, plainRefs(part, base+seg, true)...)
			} else {
				out = append(out, proseRefs(part, base+seg)...)
			}
			inCode = !inCode
			seg = i + 1
		}
	}
	return out
}

// proseRefs is the references in text outside code: markdown links, then
// plain paths and includes between them.
func proseRefs(s string, base int) []SkillRef {
	var out []SkillRef
	prev := 0
	for _, m := range mdLinkRe.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, plainRefs(s[prev:m[0]], base+prev, false)...)
		prev = m[1]
		// An image, or a link out of the skill, is left as it is.
		if t, ok := localTarget(s[m[4]:m[5]]); ok && (m[0] == 0 || s[m[0]-1] != '!') {
			out = append(out, SkillRef{Start: base + m[0], End: base + m[1], Target: t, Text: s[m[2]:m[3]]})
		}
	}
	return append(out, plainRefs(s[prev:], base+prev, false)...)
}

// localTarget is a markdown link's target when it names a file in the
// skill: not a URL, an anchor or an absolute path.
func localTarget(t string) (string, bool) {
	if strings.Contains(t, "://") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "/") ||
		strings.HasPrefix(t, "~") || strings.HasPrefix(t, "mailto:") {
		return "", false
	}
	t, _, _ = strings.Cut(t, "#")
	return t, t != ""
}

// plainRefs is the bare paths in s, and (outside code) its @includes.
func plainRefs(s string, base int, code bool) []SkillRef {
	var out []SkillRef
	for _, m := range pathTokRe.FindAllStringIndex(s, -1) {
		if m[0] > 0 && !strings.ContainsRune(tokenStart, rune(s[m[0]-1])) {
			continue
		}
		tok := strings.TrimRight(s[m[0]:m[1]], ".,:;!?")
		end := m[0] + len(tok)
		if inc, ok := strings.CutPrefix(tok, "@"); ok {
			if code || inc == "" || !strings.Contains(inc, "/") && !pathExtRe.MatchString(inc) {
				continue
			}
			out = append(out, SkillRef{Include: true, Start: base + m[0], End: base + end, Target: inc, Text: tok})
			continue
		}
		if !pathExtRe.MatchString(tok) || strings.Trim(tok, "./") == "" {
			continue
		}
		out = append(out, SkillRef{Start: base + m[0], End: base + end, Target: tok, Text: tok, Bare: !strings.Contains(tok, "/")})
	}
	return out
}

// SkillIncludePath is an include's file, relative to the skill's folder;
// false when it would leave the folder.
func SkillIncludePath(target string) (string, bool) {
	return skillRel(target)
}

// SkillLinkPath is the file a link in the file from names, relative to the
// skill's folder: beside from first, then from the folder; with neither
// there, the first (and exists says so). False: it leaves the folder.
func SkillLinkPath(from, target string, exists func(string) bool) (string, bool) {
	near, ok1 := skillRel(path.Join(path.Dir(from), target))
	root, ok2 := skillRel(target)
	switch {
	case ok1 && exists(near):
		return near, true
	case ok2 && exists(root):
		return root, true
	case ok1:
		return near, true
	}
	return root, ok2
}

// skillRel cleans a path relative to a skill's folder; false when it is
// absolute or climbs out.
func skillRel(p string) (string, bool) {
	p = filepath.ToSlash(p)
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return "", false
	}
	p = path.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") || p == "." {
		return "", false
	}
	return p, true
}

// SkillFileView is one file of a skill, for a reader: SKILL.md's front
// matter apart from its body, every file in the folder (to tell a link from
// a broken one), and the text of the files it includes, by path.
type SkillFileView struct {
	Skill, File       string
	Name, Description string // SKILL.md's front matter
	Text              string
	Clipped           bool
	Files             []string
	Includes          map[string]string
}

var errOutside = errors.New("the path is outside the skill's folder")

// SkillFile reads one file of a skill for a person: "" or "SKILL.md" is the
// skill itself. The path is confined to the skill's folder (a symlink out
// of it is refused too) and cut at skillViewMax.
func (h *Harness) SkillFile(ctx context.Context, name, file string) (SkillFileView, error) {
	v := SkillFileView{Skill: name, File: "SKILL.md", Includes: map[string]string{}}
	if file != "" {
		p, ok := skillRel(file)
		if !ok {
			return v, errOutside
		}
		v.File = p
	}
	var s *Skill
	for _, k := range h.Skills() {
		if k.Name == name {
			s = &k
			break
		}
	}
	if s == nil {
		return v, fmt.Errorf("no skill %q", name)
	}
	read := func(rel string) (string, bool, error) { return readSkillLocal(s.Dir, rel) }
	if s.Server != "" {
		read = func(rel string) (string, bool, error) {
			cs, err := h.mgr.ReadResource(ctx, s.Server, uriDir(s.URI)+"/"+rel)
			if err != nil {
				return "", false, err
			}
			var b strings.Builder
			for _, c := range cs {
				b.WriteString(c.Text)
			}
			t := b.String()
			if len(t) > skillViewMax {
				return t[:skillViewMax], true, nil
			}
			return t, false, nil
		}
		v.Files = append([]string{"SKILL.md"}, s.Files...)
	} else {
		v.Files = skillFiles(s.Dir)
	}
	text, clipped, err := read(v.File)
	if err != nil {
		return v, err
	}
	v.Clipped = clipped
	if v.File == "SKILL.md" {
		meta, body := splitFrontmatter(text)
		v.Name, v.Description, text = meta["name"], strings.Join(strings.Fields(meta["description"]), " "), body
	}
	v.Text = text
	if !isMarkdownPath(v.File) {
		return v, nil
	}
	// The included files, to SkillIncludeDepth, each once.
	total := 0
	var walk func(text string, depth int)
	walk = func(text string, depth int) {
		if depth > SkillIncludeDepth {
			return
		}
		for _, r := range SkillRefs(text) {
			if !r.Include {
				continue
			}
			p, ok := SkillIncludePath(r.Target)
			if _, seen := v.Includes[p]; !ok || seen || p == v.File || total >= skillIncludesMax {
				continue
			}
			t, _, err := read(p)
			if err != nil {
				continue // missing: the reader shows it as not found
			}
			total += len(t)
			v.Includes[p] = t
			if isMarkdownPath(p) {
				walk(t, depth+1)
			}
		}
	}
	walk(text, 1)
	return v, nil
}

// isMarkdownPath is a file a reader renders as markdown; any other is shown
// as code.
func isMarkdownPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown", ".txt", "":
		return true
	}
	return false
}

// readSkillLocal reads rel in dir, refusing a path that resolves (through a
// symlink) outside dir.
func readSkillLocal(dir, rel string) (string, bool, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, err
	}
	full, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", false, err
	}
	if r, err := filepath.Rel(root, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false, errOutside
	}
	f, err := os.Open(full)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a file", rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, skillViewMax+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > skillViewMax {
		return string(b[:skillViewMax]), true, nil
	}
	return string(b), false, nil
}

// skillFiles is every file in a local skill's folder, relative to it.
func skillFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(out) >= skillViewFilesMax {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, err := filepath.Rel(dir, p); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}
