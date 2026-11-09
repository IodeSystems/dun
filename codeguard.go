package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"

	"github.com/iodesystems/dun/internal/numfmt"
)

// The poly-lsp side of the root guard (rootguard.go): a too-big root starts
// poly-lsp on an empty placeholder, and a node_* call that names a path in
// one repository moves it onto that repository.

// codeGuard is one poly-lsp server under the root guard. Owned by the
// harness that runs the server (restartOwner).
type codeGuard struct {
	// mu serializes the server's calls: a call that moves the server to
	// another repository must not stop it under another call.
	mu   sync.Mutex
	ws   string   // the root the spec names (--root), absolute
	scan RootScan // what the guard found there
	// scope is the repository the server runs on, relative to ws; "" is the
	// placeholder. Guarded by the harness's srvMu.
	scope string
	said  bool // the start line was said
	pats  []repoPattern
}

// repoPattern finds a path inside one repository in a tool argument.
type repoPattern struct {
	repo string
	re   *regexp.Regexp // group 1: what precedes the path; the match ends after "<repo>/"
}

// codeGuardOf is id's guard, nil when the root is not guarded.
func (h *Harness) codeGuardOf(id string) *codeGuard {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.codeGuards[id]
}

// placeholderRoot is the empty directory a guarded poly-lsp starts on.
func placeholderRoot() (string, error) {
	d := filepath.Join(DunHome(), "poly-lsp-empty-root")
	return d, os.MkdirAll(d, 0o700)
}

// rootArg is the index of --root's value in args, -1 when there is none
// ("--root=x" is split in place).
func rootArg(args []string) int {
	for i, a := range args {
		if a == "--root" && i+1 < len(args) {
			return i + 1
		}
	}
	return -1
}

// guardCodeRoot applies the root guard to a poly-lsp start: args with --root
// on the guard's repository or the placeholder, and the excludes that apply
// there. A root that is not too big, a spec with no --root, and a project
// whose person set index-anyway are not guarded.
func (h *Harness) guardCodeRoot(s Server, args, excludes []string) ([]string, []string) {
	i := rootArg(args)
	if i < 0 {
		return args, excludes
	}
	ws := args[i]
	if !filepath.IsAbs(ws) {
		ws = filepath.Join(h.cfg.Workspace, ws)
	}
	ws = filepath.Clean(ws)
	scan := RootScan{}
	if !s.IndexAnyway {
		scan = scanRootCached(ws)
	}
	h.srvMu.Lock()
	g := h.codeGuards[s.ID]
	if s.IndexAnyway || !scan.TooBig() {
		delete(h.codeGuards, s.ID)
		h.srvMu.Unlock()
		return args, excludes
	}
	if g == nil || g.ws != ws {
		g = &codeGuard{ws: ws, scan: scan, pats: repoPatterns(ws, scan.Repos)}
		if h.codeGuards == nil {
			h.codeGuards = map[string]*codeGuard{}
		}
		h.codeGuards[s.ID] = g
	}
	scope, said := g.scope, g.said
	g.said = true
	h.srvMu.Unlock()
	args = slices.Clone(args)
	if scope == "" {
		p, err := placeholderRoot()
		if err != nil {
			p = os.TempDir()
		}
		args[i] = p
		if !said {
			h.noteServer(s.ID, "%s holds %s: not indexed whole (dun's root guard); poly-lsp starts on a repository when a node_* call names a path in it",
				h.tildePath(ws), scanSummary(scan))
		}
		return args, nil
	}
	args[i] = filepath.Join(ws, filepath.FromSlash(scope))
	var ex []string
	for _, e := range excludes {
		if rest, ok := strings.CutPrefix(cleanExclude(e), scope+"/"); ok {
			ex = append(ex, rest)
		}
	}
	h.noteServer(s.ID, "running on %s/ (dun's root guard)", scope)
	return args, ex
}

// scanSummary is "12 git repos (over 50,000 files)".
func scanSummary(r RootScan) string {
	files := numfmt.Grouped(r.Files) + " files"
	if r.Over {
		files = "over " + numfmt.Grouped(guardMaxFiles) + " files"
		if r.TimedOut {
			files = "more files than dun could count in " + guardScanTimeout.String()
		}
	}
	switch n := len(r.Repos); {
	case r.InRepo:
		return "1 repo (" + files + ")"
	case n == 1:
		return "1 git repo (" + files + ")"
	default:
		return fmt.Sprintf("%d git repos (%s)", n, files)
	}
}

// repoPatterns are the patterns that find a path inside each repo: "<repo>/"
// or "<ws>/<repo>/" at the start of a path (after a quote, =, #, space, or
// the start of the string).
func repoPatterns(ws string, repos []string) []repoPattern {
	out := make([]repoPattern, 0, len(repos))
	for _, r := range repos {
		re := regexp.MustCompile(`(^|[^\w.\-/])(?:` + regexp.QuoteMeta(filepath.ToSlash(ws)) + `/)?` + regexp.QuoteMeta(r) + `(?:/|$)`)
		out = append(out, repoPattern{repo: r, re: re})
	}
	return out
}

// reposNamed are the repositories the strings name a path inside, the
// deepest one where two nest ("a" and "a/b" for "a/b/x.go": "a/b").
func (g *codeGuard) reposNamed(strs []string) []string {
	set := map[string]bool{}
	for _, s := range strs {
		at := map[int]string{} // where a path starts → the longest repo there
		for _, p := range g.pats {
			for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
				if cur, ok := at[m[3]]; !ok || len(p.repo) > len(cur) {
					at[m[3]] = p.repo
				}
			}
		}
		for _, r := range at {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// rewrite is s with paths inside repo made relative to it: "repo/x" and
// "<ws>/repo/x" become "x", and "repo" alone becomes ".".
func (g *codeGuard) rewrite(s, repo string) string {
	for _, p := range g.pats {
		if p.repo != repo {
			continue
		}
		return p.re.ReplaceAllStringFunc(s, func(m string) string {
			sub := p.re.FindStringSubmatch(m)
			if strings.HasSuffix(m, "/") {
				return sub[1]
			}
			return sub[1] + "."
		})
	}
	return s
}

// argStrings are every string in v, depth first.
func argStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = argStrings(x[k], out)
		}
	case []any:
		for _, e := range x {
			out = argStrings(e, out)
		}
	}
	return out
}

// mapStrings is v with f applied to every string in it.
func mapStrings(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = mapStrings(e, f)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = mapStrings(e, f)
		}
		return out
	}
	return v
}

// guardRepoLines is how many repositories a refusal names.
const guardRepoLines = 8

// refusal is what a call the guard cannot place reads.
func (g *codeGuard) refusal(named []string) string {
	if g.scan.InRepo || len(g.scan.Repos) == 0 {
		return fmt.Sprintf("ERROR: this folder holds %s: poly-lsp is off here (dun's root guard, over %s files). "+
			"Find code with exec (grep) or search instead. The person can run /lsp index-anyway on to index it whole.",
			scanSummary(g.scan), numfmt.Grouped(guardMaxFiles))
	}
	if len(named) > 1 {
		return fmt.Sprintf("ERROR: this call names paths in %d repos (%s); poly-lsp works on one repo at a time: one call per repo.",
			len(named), strings.Join(named, ", "))
	}
	list := g.scan.Repos[:min(len(g.scan.Repos), guardRepoLines)]
	more := ""
	if len(g.scan.Repos) > len(list) {
		more = fmt.Sprintf(", … (%d more)", len(g.scan.Repos)-len(list))
	}
	return fmt.Sprintf("ERROR: this folder holds %s; poly-lsp works on one repo: give path= inside one of: %s%s",
		scanSummary(g.scan), strings.Join(list, ", "), more)
}

// withCodeGuard places the calls to a guarded poly-lsp (codeGuard): a call
// that names a path in one repository runs on it — the server is moved
// there first when it is elsewhere — with the paths made relative to it; a
// call that names none runs where the server is, or, on the placeholder, is
// refused with the repositories to choose from.
func withCodeGuard(inner agent.ToolDispatcher, h *Harness, tools []mcpmgr.MCPTool) agent.ToolDispatcher {
	owner := map[string]string{} // model tool name → server id, for servers that index
	h.srvMu.Lock()
	for _, t := range tools {
		if s, ok := h.spec(t.ServerID); ok && asksIndex(s) {
			owner[modelToolName(t)] = t.ServerID
		}
	}
	h.srvMu.Unlock()
	if len(owner) == 0 {
		return inner
	}
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		id, ok := owner[tc.Function.Name]
		if !ok {
			return inner(ctx, tc)
		}
		o := h.restartOwner(id)
		g := o.codeGuardOf(id)
		if g == nil {
			return inner(ctx, tc)
		}
		var args any
		if s := strings.TrimSpace(tc.Function.Arguments); s != "" && s != "null" {
			if json.Unmarshal([]byte(s), &args) != nil {
				return inner(ctx, tc) // the dispatcher reports bad JSON
			}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		named := g.reposNamed(argStrings(args, nil))
		o.srvMu.Lock()
		scope := g.scope
		o.srvMu.Unlock()
		note := ""
		switch {
		case len(named) > 1 || (len(named) == 0 && scope == "") || g.scan.InRepo:
			return g.refusal(named), nil
		case len(named) == 1 && named[0] != scope:
			repo := named[0]
			if r := scanRootCached(filepath.Join(g.ws, filepath.FromSlash(repo))); r.TooBig() {
				return fmt.Sprintf("ERROR: %s/ holds %s: poly-lsp is off for it (dun's root guard, over %s files). Find code there with exec (grep) or search.",
					repo, scanSummary(r), numfmt.Grouped(guardMaxFiles)), nil
			}
			if err := o.moveCodeServer(ctx, id, g, repo); err != nil {
				return fmt.Sprintf("ERROR: poly-lsp could not start on %s/: %s", repo, oneLine(err.Error(), 300)), nil
			}
			scope = repo
			note = fmt.Sprintf("[poly-lsp now runs on %s/ (this folder holds %s): paths in its answers are relative to %s/; name a path in another repo to move it]\n",
				repo, scanSummary(g.scan), repo)
		}
		if args != nil {
			args = mapStrings(args, func(s string) string { return g.rewrite(s, scope) })
			b, _ := json.Marshal(args)
			tc.Function.Arguments = string(b)
		}
		res, err := inner(ctx, tc)
		return note + res, err
	}
}

// moveCodeServer restarts id on repo (relative to the guard's root).
func (h *Harness) moveCodeServer(ctx context.Context, id string, g *codeGuard, repo string) error {
	h.srvMu.Lock()
	prev := g.scope
	g.scope = repo
	s, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return fmt.Errorf("no server %q", id)
	}
	h.noteServer(id, "a node_* call names %s/: restarting on it", repo)
	h.mgrFor(id).StopServer(id)
	if err := h.startServer(ctx, s); err != nil {
		h.srvMu.Lock()
		g.scope = prev
		h.srvMu.Unlock()
		return err
	}
	return nil
}
