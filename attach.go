package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Attached indexes (plan item 20): index a directory once, and let many
// sessions read it.
//
// raglit's daemon is shared, so an index one project fills serves every
// session that names it. `/rag watch <dir> [--index N]` has raglit watch
// a directory into an index of this project (raglit keeps it fresh; dun only
// reads it). `/rag attach <index>` adds it to what this session's search
// reads: the model's search and get calls that name no index, and the
// proactive search (turn.before).
//
// An index is "project:index", a bare index being this project's. Each scope
// — the session's .mcp.json, the project's dun.local.json, the global
// mcp.json, under the extension's server id — may switch it on (attach) or
// off (detach), and the nearest scope that names it decides, as /mcp's
// switches do (USER 2026-10-28: /rag). Unset everywhere, this project's own
// indexes are on and every other one off.
//
// With another project's index on, the search's `index` argument is
// "<own>:*,<project>:<index>,…" (raglit's read selector: its own indexes,
// plus one named index of any project). With one of its own off, "<own>:*"
// becomes its own indexes named one by one, less those (the list is
// raglit's, cached: ownIndexes). raglit expands "all" only as the whole
// selector, so a selector drops the `shared` namespaces of raglit's project
// config while it is in force (risk recorded in the plan).

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

// effectiveIndexes is the indexes switched on (on) or off (!on): for each
// index a scope names, the nearest scope decides — session, then project,
// then global.
func effectiveIndexes(sc ServerScopes, on bool) []string {
	layers := []struct{ attach, detach []string }{
		{sc.AttachSession, sc.DetachSession}, {sc.AttachProject, sc.DetachProject}, {sc.AttachGlobal, sc.DetachGlobal},
	}
	var out []string
	for _, name := range unionAttach(sc.AttachGlobal, sc.AttachProject, sc.AttachSession,
		sc.DetachGlobal, sc.DetachProject, sc.DetachSession) {
		for _, l := range layers {
			if slices.Contains(l.attach, name) {
				if on {
					out = append(out, name)
				}
				break
			}
			if slices.Contains(l.detach, name) {
				if !on {
					out = append(out, name)
				}
				break
			}
		}
	}
	return out
}

// attachSelector is the `index` argument a search with no index of its own
// gets: own's indexes (all of them, "<own>:*", or ownIdx less the ones
// switched off) plus the switched-on ones of other projects; "" when that
// is just own's own (the server's default reads them already).
func attachSelector(own string, attach, detach, ownIdx []string) string {
	var parts []string
	ownOff := false
	for _, d := range detach {
		if p, _, _ := strings.Cut(d, ":"); p == own {
			ownOff = true
		}
	}
	switch {
	case !ownOff || len(ownIdx) == 0: // nothing off, or own's list unknown yet
		parts = append(parts, own+":*")
	default:
		for _, i := range ownIdx {
			if !slices.Contains(detach, i) {
				parts = append(parts, i)
			}
		}
	}
	for _, a := range attach {
		if p, _, _ := strings.Cut(a, ":"); p != own {
			parts = append(parts, a)
		}
	}
	if !ownOff && len(parts) == 1 {
		return ""
	}
	if len(parts) == 0 {
		return own + ":-" // every index of its own off, nothing else on: no index raglit has
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

// AttachIndex attaches (on) or detaches (off) index for server id's search
// at scope (SetIndex).
func (h *Harness) AttachIndex(ctx context.Context, id, index, scope string, on bool) (string, string, bool, error) {
	return h.SetIndex(ctx, id, index, scope, &on)
}

// SetIndex switches index on or off for server id's search at scope, or
// unsets it there (nil). It returns the index as stored, the file written
// ("" when nothing changed or the session is in memory) and whether it
// changed. The model's tools and the proactive search follow at once.
func (h *Harness) SetIndex(ctx context.Context, id, index, scope string, on *bool) (string, string, bool, error) {
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
	var att, det []string
	switch scope {
	case ScopeGlobal:
		att, det = s.Scopes.AttachGlobal, s.Scopes.DetachGlobal
	case ScopeProject:
		att, det = s.Scopes.AttachProject, s.Scopes.DetachProject
	case ScopeSession:
		att, det = s.Scopes.AttachSession, s.Scopes.DetachSession
	default:
		return "", "", false, fmt.Errorf("scope %q: want global, project or session", scope)
	}
	without := func(l []string) []string {
		var out []string
		for _, a := range l {
			if a != name {
				out = append(out, a)
			}
		}
		return out
	}
	nextAtt, nextDet := without(att), without(det)
	switch {
	case on != nil && *on:
		nextAtt = append(nextAtt, name)
	case on != nil:
		nextDet = append(nextDet, name)
	}
	if slices.Equal(nextAtt, att) && slices.Equal(nextDet, det) {
		return name, "", false, nil
	}
	set := func(a, d *[]string) { *a, *d = nextAtt, nextDet }
	path, err := h.writeScope(id, scope,
		func(sp *ServerSpec) { set(&sp.Attach, &sp.Detach) },
		func(sp *ServerSpec) { set(&sp.Attach, &sp.Detach) },
		func(ss *SessionServer) { set(&ss.Attach, &ss.Detach) })
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
		if !ok || len(s.Attach)+len(s.Detach) == 0 || !toolTakes(tools, e.ID, e.Roles.Search.Tool, "index") {
			continue
		}
		own := h.ownProject(s)
		ownIdx := h.ownIndexes(e, s, own)
		if sel := attachSelector(own, s.Attach, s.Detach, ownIdx); sel != "" {
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
		var attach, detach []string
		for _, s := range specs {
			if s.ID == e.ID {
				attach, detach = s.Attach, s.Detach
			}
		}
		var say []string
		if len(attach) > 0 {
			say = append(say, "searched with this project's: "+strings.Join(attach, ", "))
		}
		if len(detach) > 0 {
			say = append(say, "switched off: "+strings.Join(detach, ", "))
		}
		name := modelToolName(mcpmgr.MCPTool{ServerID: e.ID, Name: e.Roles.Search.Tool})
		for i := range defs {
			if defs[i].Function.Name == name {
				defs[i].Function.Description += " When `index` is empty — indexes " + strings.Join(say, "; ") +
					". Name one (\"project:index\") in `index` to search only it."
			}
		}
	}
}

// ownIdxCache is a project's own indexes, as raglit listed them, by project:
// what a selector names one by one when one of them is switched off.
// Listing runs the index CLI (seconds), so it never runs under the lock a
// tool rebuild holds: a miss starts it in the background and the rebuild
// after it uses the list.
var ownIdxCache, ownIdxListing sync.Map // project → []string ("project:index"); project → listing now

// ownIndexes is own's indexes, qualified, or nil until they are known.
func (h *Harness) ownIndexes(e Extension, s Server, own string) []string {
	if v, ok := ownIdxCache.Load(own); ok {
		return v.([]string)
	}
	cli := "raglit"
	if e.Roles.Index != nil && e.Roles.Index.CLI != "" {
		cli = e.Roles.Index.CLI
	}
	if _, busy := ownIdxListing.LoadOrStore(own, true); busy {
		return nil
	}
	ws := h.cfg.Workspace
	go func() {
		defer ownIdxListing.Delete(own)
		rows, err := listIndexes(context.Background(), cli, ws)
		if err != nil {
			return
		}
		noteOwnIndexes(own, rows)
		h.applyTools()
	}()
	return nil
}

// noteOwnIndexes caches own's indexes from a listing.
func noteOwnIndexes(own string, rows []raglitIndexRow) {
	var idx []string
	for _, r := range rows {
		if strings.ToLower(r.Project) == own && r.Index != "" {
			idx = append(idx, own+":"+strings.ToLower(r.Index))
		}
	}
	ownIdxCache.Store(own, idx)
}
