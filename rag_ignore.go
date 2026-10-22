package dun

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// globRules is a gitignore-syntax rule set: comments, blank lines, `!`
// negation, trailing-`/` directory rules, `/`-anchored rules, `*`, `?`,
// `[…]` and `**`. Rules from a nested .gitignore carry their directory as
// base and apply only beneath it. The last matching rule wins.
//
// A port of agentkit/files' matcher, which is unexported. Used where git
// cannot answer: .gitignore files in a workspace that is not a git repo, and
// the "rag" section's include and exclude patterns.
type globRules struct {
	rules []globRule
}

type globRule struct {
	base    string // slash dir the rule's file lives in, "" for the root
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	// prefix is the literal start of an anchored pattern ("node_modules/foo/"
	// for "node_modules/foo/*.js"); "" for a pattern that matches at any depth.
	prefix string
}

func newGlobRules(base string, lines []string) *globRules {
	g := &globRules{}
	g.add(base, lines)
	return g
}

func (g *globRules) add(base string, lines []string) {
	for _, l := range lines {
		if r, ok := parseGlobRule(base, l); ok {
			g.rules = append(g.rules, r)
		}
	}
}

// addFile reads one ignore file in directory base. A missing file adds nothing.
func (g *globRules) addFile(base, abs string) {
	f, err := os.Open(abs)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	g.add(base, lines)
}

func parseGlobRule(base, line string) (globRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return globRule{}, false
	}
	r := globRule{base: base}
	switch {
	case strings.HasPrefix(line, "!"):
		r.negate = true
		line = line[1:]
	case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return globRule{}, false
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	expr := "^(?:.*/)?" + globRegex(line) + "$"
	if anchored {
		expr = "^" + globRegex(line) + "$"
		lit := line
		if i := strings.IndexAny(lit, `*?[\`); i >= 0 {
			lit = lit[:i]
		} else {
			lit += "/"
		}
		if base != "" {
			lit = base + "/" + lit
		}
		r.prefix = lit
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return globRule{}, false // a malformed rule matches nothing, as in git
	}
	r.re = re
	return r, true
}

// globRegex translates a gitignore glob to a regexp body.
func globRegex(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && strings.HasPrefix(g[i:], "**"):
			atStart := i == 0 || g[i-1] == '/'
			switch {
			case atStart && strings.HasPrefix(g[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 2
			case atStart && i+2 == len(g):
				b.WriteString(".*")
				i++
			default:
				b.WriteString("[^/]*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(g):
			i++
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		case c == '[':
			end := strings.IndexByte(g[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// match is the verdict of the last rule matching rel (a slash path from the
// root) itself.
func (g *globRules) match(rel string, isDir bool) bool {
	ignored := false
	for _, r := range g.rules {
		if r.dirOnly && !isDir {
			continue
		}
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = rel[len(r.base)+1:]
		}
		if r.re.MatchString(sub) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matches reports whether rel is matched by itself or through a matched
// ancestor directory ("data/" covers everything beneath data).
func (g *globRules) matches(rel string, isDir bool) bool {
	if g == nil || len(g.rules) == 0 {
		return false
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && g.match(rel[:i], true) {
			return true
		}
	}
	return g.match(rel, isDir)
}

// reaches reports whether a rule could match something beneath dir: dir is
// matched, or an anchored rule names a path inside it. A pattern with no
// slash ("*.json") matches at any depth and does not count — honoring it
// would mean walking every node_modules to look.
func (g *globRules) reaches(dir string) bool {
	if g.matches(dir, true) {
		return true
	}
	if g == nil {
		return false
	}
	for _, r := range g.rules {
		if !r.negate && r.prefix != "" && strings.HasPrefix(r.prefix, dir+"/") {
			return true
		}
	}
	return false
}

// anchored is the rules that name a path ("node_modules/foo/"), without the
// ones that match at any depth ("*.json"): what may reach inside a junk
// directory.
func (g *globRules) anchored() *globRules {
	if g == nil {
		return nil
	}
	out := &globRules{}
	for _, r := range g.rules {
		if r.prefix != "" {
			out.rules = append(out.rules, r)
		}
	}
	return out
}
