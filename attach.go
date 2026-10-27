package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Attached indexes (plan item 20): index a directory once, and let many
// sessions read it.
//
// raglit's daemon is shared, so an index one project fills serves every
// session that names it. `/raglit watch <dir> [--index N]` has raglit watch
// a directory into an index of this project (raglit keeps it fresh; dun only
// reads it). `/raglit attach <index>` adds it to what this session's search
// reads: the model's search and get calls that name no index, and the
// proactive search (turn.before).
//
// An attachment is "project:index", a bare index being this project's. It is
// kept at the same three scopes as the server switches (mcpscope.go) — the
// session's .mcp.json, the project's dun.local.json, the global mcp.json —
// under the extension's server id, and the scopes ADD UP: an index attached
// anywhere is read.
//
// With another project's index attached, the search's `index` argument is
// "<own>:*,<project>:<index>,…" (raglit's read selector: its own indexes,
// plus one named index of any project). raglit expands "all" only as the
// whole selector, so this selector drops the `shared` namespaces of raglit's
// project config while something is attached (risk recorded in the plan).
// Attachments of this project's own indexes change nothing: "<own>:*" reads
// them already.

// attachName is what an attachment may be once qualified.
// ":<index>" is a global index (raglit 359f049).
var attachName = regexp.MustCompile(`^[a-z0-9_-]*:[a-z0-9_-]+$`)

// qualifyAttach is index as stored: lowercased, "<own>:" in front of a bare
// name. An error names what is wrong with it.
func qualifyAttach(own, index string) (string, error) {
	index = strings.ToLower(strings.TrimSpace(index))
	if !strings.Contains(index, ":") {
		index = own + ":" + index
	}
	if !attachName.MatchString(index) {
		return "", fmt.Errorf("index %q: want <index> or <project>:<index> (letters, digits, - and _)", index)
	}
	return index, nil
}

// unionAttach is the scopes' attachments, each once, in scope order.
func unionAttach(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, a := range l {
			if a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// argValue is the value of --name (or --name=v, -name) in args, or "".
func argValue(args []string, name string) string {
	for i, a := range args {
		k, v, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || k != name {
			continue
		}
		if hasValue {
			return v
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ownProject is the project namespace a server's search reads as its own:
// its --project, else the workspace's corpus name ({{project}}).
func (h *Harness) ownProject(s Server) string {
	if p := strings.TrimSpace(argValue(s.Args, "project")); p != "" {
		return strings.ToLower(p)
	}
	return raglitProject(h.cfg.Workspace)
}

// AttachName is index as server id stores an attachment: "<own>:index" for a
// bare name; index as given when it does not qualify.
func (h *Harness) AttachName(id, index string) string {
	s, err := h.knownServer(id)
	if err != nil {
		return index
	}
	if name, err := qualifyAttach(h.ownProject(s), index); err == nil {
		return name
	}
	return index
}

// attachSelector is the `index` argument a search with no index of its own
// gets: own's indexes plus the attached ones of other projects; "" when
// there are none (the server's default reads its own already).
func attachSelector(own string, attach []string) string {
	parts := []string{own + ":*"}
	for _, a := range attach {
		if p, _, _ := strings.Cut(a, ":"); p != own {
			parts = append(parts, a)
		}
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, ",")
}

// Attachment is one attached index and the scopes it is attached at.
type Attachment struct {
	Index  string
	Scopes []string
}

// Attachments are server id's attached indexes, with their scopes.
func (h *Harness) Attachments(id string) ([]Attachment, error) {
	s, err := h.knownServer(id)
	if err != nil {
		return nil, err
	}
	var out []Attachment
	for _, a := range s.Attach {
		at := Attachment{Index: a}
		for _, sc := range []struct {
			name string
			list []string
		}{{ScopeSession, s.Scopes.AttachSession}, {ScopeProject, s.Scopes.AttachProject}, {ScopeGlobal, s.Scopes.AttachGlobal}} {
			for _, x := range sc.list {
				if x == a {
					at.Scopes = append(at.Scopes, sc.name)
					break
				}
			}
		}
		out = append(out, at)
	}
	return out, nil
}

// AttachIndex attaches (on) or detaches index for server id's search at
// scope. It returns the attachment as stored, the file written ("" when
// nothing changed or the session is in memory) and whether it changed. The
// model's tools and the proactive search follow at once.
func (h *Harness) AttachIndex(ctx context.Context, id, index, scope string, on bool) (string, string, bool, error) {
	e, ok := h.extension(id)
	if !ok || e.Roles.Search == nil {
		return "", "", false, fmt.Errorf("%s has no search role to attach an index to", id)
	}
	s, err := h.knownServer(id)
	if err != nil {
		return "", "", false, err
	}
	name, err := qualifyAttach(h.ownProject(s), index)
	if err != nil {
		return "", "", false, err
	}
	var cur []string
	switch scope {
	case ScopeGlobal:
		cur = s.Scopes.AttachGlobal
	case ScopeProject:
		cur = s.Scopes.AttachProject
	case ScopeSession:
		cur = s.Scopes.AttachSession
	default:
		return "", "", false, fmt.Errorf("scope %q: want global, project or session", scope)
	}
	var next []string
	had := false
	for _, a := range cur {
		if a == name {
			had = true
			continue
		}
		next = append(next, a)
	}
	if on {
		next = append(next, name)
	}
	if had == on {
		return name, "", false, nil
	}
	edit := func(l *[]string) { *l = next }
	path, err := h.writeScope(id, scope,
		func(sp *ServerSpec) { edit(&sp.Attach) },
		func(sp *ServerSpec) { edit(&sp.Attach) },
		func(ss *SessionServer) { edit(&ss.Attach) })
	if err != nil {
		return name, "", false, err
	}
	return name, path, true, h.reloadServers(ctx, true, nil)
}

// WatchDir has server id's index CLI (roles.index.cli) watch dir into index
// of this project: raglit's `watch DIR --index NAME`, which creates the index,
// records the root in raglit's config and queues what it has not seen. It
// returns the index and the CLI's output. A relative dir is the workspace's;
// index "" is the directory's name.
func (h *Harness) WatchDir(ctx context.Context, id, dir, index string) (string, string, error) {
	e, ok := h.extension(id)
	if !ok || e.Roles.Index == nil || e.Roles.Index.CLI == "" {
		return "", "", fmt.Errorf("%s has no index CLI to watch a directory with", id)
	}
	s, err := h.knownServer(id)
	if err != nil {
		return "", "", err
	}
	dir = absPath(dir, h.cfg.Workspace)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("%s is not a directory", dir)
	}
	if index == "" {
		index = normalizeProject(filepath.Base(dir))
	}
	// raglit's spelling of the name (NormalizeIndexName), so the name
	// reported is the one to attach.
	index = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return -1
	}, strings.ToLower(index))
	if index == "" {
		return "", "", fmt.Errorf("an index name is letters, digits, - and _")
	}
	out, err := h.runIndexCLI(ctx, e, s, []string{"watch", dir, "--index", index})
	return index, out, err
}

// attachSelectors are the selectors of the search extensions whose search
// tool takes an `index`, by extension id.
func (h *Harness) attachSelectors(exts []Extension, tools []mcpmgr.MCPTool) map[string]string {
	out := map[string]string{}
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	for _, e := range exts {
		s, ok := h.spec(e.ID)
		if !ok || len(s.Attach) == 0 || !toolTakes(tools, e.ID, e.Roles.Search.Tool, "index") {
			continue
		}
		if sel := attachSelector(h.ownProject(s), s.Attach); sel != "" {
			out[e.ID] = sel
		}
	}
	return out
}

// attachedTools are the model's names of the search and get tools that take
// the selector, mapped to it.
func attachedTools(exts []Extension, tools []mcpmgr.MCPTool, sel map[string]string) map[string]string {
	out := map[string]string{}
	for _, e := range exts {
		s := sel[e.ID]
		if s == "" {
			continue
		}
		for _, name := range []string{e.Roles.Search.Tool, e.Roles.Search.Get} {
			if name != "" && toolTakes(tools, e.ID, name, "index") {
				out[modelToolName(mcpmgr.MCPTool{ServerID: e.ID, Name: name})] = s
			}
		}
	}
	return out
}

// withAttached gives a call to one of byTool's tools that names no index the
// tool's selector, so the model's search reads the attached indexes too.
func withAttached(next agent.ToolDispatcher, byTool map[string]string) agent.ToolDispatcher {
	if len(byTool) == 0 {
		return next
	}
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		sel, ok := byTool[tc.Function.Name]
		if !ok {
			return next(ctx, tc)
		}
		var args map[string]any
		if s := strings.TrimSpace(tc.Function.Arguments); s != "" && s != "null" {
			if json.Unmarshal([]byte(s), &args) != nil {
				return next(ctx, tc) // the dispatcher explains bad JSON
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		if idx, _ := args["index"].(string); strings.TrimSpace(idx) != "" {
			return next(ctx, tc)
		}
		args["index"] = sel
		b, _ := json.Marshal(args)
		tc.Function.Arguments = string(b)
		return next(ctx, tc)
	}
}

// describeAttached appends the attached indexes to each search tool's
// description, so the model knows what an unscoped search reads.
func describeAttached(defs []llm.ToolDef, exts []Extension, specs []Server, sel map[string]string) {
	for _, e := range exts {
		if sel[e.ID] == "" {
			continue
		}
		var attach []string
		for _, s := range specs {
			if s.ID == e.ID {
				attach = s.Attach
			}
		}
		name := modelToolName(mcpmgr.MCPTool{ServerID: e.ID, Name: e.Roles.Search.Tool})
		for i := range defs {
			if defs[i].Function.Name == name {
				defs[i].Function.Description += " Attached indexes, searched with this project's when `index` is empty: " +
					strings.Join(attach, ", ") + ". Name one (\"project:index\") in `index` to search only it."
			}
		}
	}
}
