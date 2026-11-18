package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/memory"
	"github.com/iodesystems/dun/proto"
)

// The "indexes" switch kind (USER 2026-10-27, /rag 2026-10-28): /rag is the
// switch screen of every index raglit has, of every project, searchable,
// with each one on, off or unset at the global, project and session scopes;
// the nearest scope that says decides (attach.go). Unset everywhere, this
// project's own indexes are on and every other one off. The list is
// raglit's `index --list --json`; indexes a scope names that it does not
// list are shown too. Above raglit's rows is dund's project index
// (projectIndexRow), which has no switch: it is in every session.

const indexesKind = "indexes"

// raglitIndex is a row of raglit's `index --list --json` (raglit 8ea90c8).
type raglitIndexRow struct {
	Selector  string   `json:"selector"` // "dun:dun-main", ":code" for a global one
	Project   string   `json:"project"`
	Index     string   `json:"index"`
	Documents int      `json:"documents"`
	Root      string   `json:"root"`
	Scopes    []string `json:"scopes"`
	Ambient   bool     `json:"ambient"`
	About     string   `json:"about"`
	Meaning   bool     `json:"meaning"`
}

// listIndexes runs the search extension's index CLI for every index.
var listIndexes = func(ctx context.Context, cli, dir string, env []string) ([]raglitIndexRow, error) {
	p, err := mcpserver.FindCommandContext(ctx, cli)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "index", "--list", "--json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s index --list --json: %s", cli, strings.TrimSpace(firstLine(string(ee.Stderr))))
		}
		return nil, fmt.Errorf("%s index --list --json: %w", cli, err)
	}
	var v struct {
		Indexes []raglitIndexRow `json:"indexes"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("%s index --list --json: %w (a raglit older than 8ea90c8 has no --json)", cli, err)
	}
	return v.Indexes, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// indexesKind is the kind; it has no modes.
func (h *Harness) indexesSwitchKind() switchKind {
	return switchKind{name: indexesKind, slash: "rag", view: h.indexesView, set: h.setIndexAttach}
}

func (h *Harness) indexesView() SwitchView {
	v := SwitchView{Kind: indexesKind, Slash: "rag", Modes: map[string]string{}}
	// dund's project index first (USER 2026-11-17: "dund default, raglit
	// add-on"): always on, so it has no switches; raglit is the add-on below.
	v.Items = append(v.Items, h.projectIndexRow())
	s, err := h.knownServer(mcpserver.ServerDocs)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	own := h.ownProject(s)
	// The provider first (USER 2026-10-28): raglit itself, its switch the
	// same as /mcp's, the indexes under it.
	provider := SwitchState{SwitchItem: SwitchItem{Name: mcpserver.ServerDocs, Description: h.describe(mcpserver.ServerDocs)},
		On: s.Autostart, From: s.Scopes.OnFrom,
		Global: mcpserver.OnOffWord(s.Scopes.GlobalOn(s.Via)), Project: tri(s.Scopes.Project), Session: tri(s.Scopes.Session)}
	v.Items = append(v.Items, provider)
	pi := len(v.Items) - 1
	scopes := map[string]map[string]string{}
	mark := func(list []string, scope, val string) {
		for _, n := range list {
			if scopes[n] == nil {
				scopes[n] = map[string]string{}
			}
			scopes[n][scope] = val
		}
	}
	sc := s.Scopes
	mark(sc.AttachGlobal, mcpserver.ScopeGlobal, "on")
	mark(sc.DetachGlobal, mcpserver.ScopeGlobal, "off")
	mark(sc.AttachProject, mcpserver.ScopeProject, "on")
	mark(sc.DetachProject, mcpserver.ScopeProject, "off")
	mark(sc.AttachSession, mcpserver.ScopeSession, "on")
	mark(sc.DetachSession, mcpserver.ScopeSession, "off")
	seen := map[string]bool{}
	add := func(it SwitchItem) {
		seen[it.Name] = true
		m := scopes[it.Name]
		it.Parent = mcpserver.ServerDocs
		st := SwitchState{SwitchItem: it, Global: m[mcpserver.ScopeGlobal], Project: m[mcpserver.ScopeProject], Session: m[mcpserver.ScopeSession]}
		p, _, _ := strings.Cut(it.Name, ":")
		st.On = p == own // unset everywhere: this project's own are on
		for _, scope := range switchScopes {
			if v := m[scope]; v != "" {
				st.On, st.From = v == "on", scope
				break
			}
		}
		v.Items = append(v.Items, st)
	}
	cli := "raglit"
	if e, ok := h.extension(mcpserver.ServerDocs); ok && e.Roles.Index != nil && e.Roles.Index.CLI != "" {
		cli = e.Roles.Index.CLI
	}
	rows, err := listIndexes(context.Background(), cli, h.cfg.Workspace, h.raglitEnv())
	if err != nil {
		v.Note = err.Error() + " — only the indexes a scope names are shown"
	} else {
		noteOwnIndexes(own, rows) // what a selector names when one of these is off
	}
	for _, r := range rows {
		name := strings.ToLower(r.Selector)
		if name == "" {
			continue
		}
		src := fmt.Sprintf("%d docs", r.Documents)
		switch {
		case r.Project == "":
			src += " · global"
		case r.Project == own:
			src += " · this project's"
		}
		desc := r.About
		if desc == "" {
			desc = r.Root
		}
		var det []string
		if r.Root != "" {
			det = append(det, "root "+r.Root)
		}
		if r.Project == own {
			det = append(det, "this project's own: on unless a scope switches it off")
		}
		if r.Ambient {
			det = append(det, "ambient: every search reads it")
		}
		if !r.Meaning {
			det = append(det, "exact-word search only (no embeddings)")
		}
		add(SwitchItem{Name: name, Description: desc, Source: src, Detail: strings.Join(det, "\n")})
	}
	for n := range scopes {
		if !seen[n] {
			add(SwitchItem{Name: n, Source: "switched", Description: "not in raglit's list"})
		}
	}
	state := ServerStateWord(ServerState{Auto: s.Autostart})
	for _, st := range h.Servers() {
		if st.ID == mcpserver.ServerDocs {
			state = ServerStateWord(st)
		}
	}
	v.Items[pi].Source = fmt.Sprintf("rag add-on (docs_search) · %s · %d indexes", state, len(v.Items)-1-pi)
	on, from := h.WorkspaceIndexed()
	if from == "" {
		from = "the default"
	}
	ws := "this workspace is read into raglit's " + own + " index (" + from + ") — /rag workspace off to stop"
	switch {
	case h.DundReadsWorkspace():
		ws = "dund reads this workspace (always), into the project index; raglit reads only the indexes switched on"
	case !on:
		ws = "this workspace is not indexed (" + from + ") — search reads the indexes switched on"
	}
	v.Status = []string{ws, "the nearest scope that switches an index decides; unset, this project's are on · ctrl+g/p/s on → off → unset"}
	return v
}

// projectIndexName is the /rag row of dund's project index.
const projectIndexName = "dund"

// dundIndexes are a project's indexes in dund (ragd's projectIndexes).
var dundIndexes = []string{"code", "docs", "memories", "strategies", "sessions"}

// projectIndexRow is dund's project index, with its link's state: dund
// answers a search now (up), does not (dund down), or the session has no
// link (DUN_MEMORY=0, off).
func (h *Harness) projectIndexRow() SwitchState {
	state := "off (DUN_MEMORY=0)"
	if h.cfg.Rag != nil {
		state = "up"
		if !h.ragUp() {
			state = "dund down"
		}
	}
	return SwitchState{SwitchItem: SwitchItem{Name: projectIndexName,
		Description: "project index (dund): this project's code and docs, notes, strategies and constraints, and folded session history; always on",
		Source:      "project index (dund) · " + state + " · " + strings.Join(dundIndexes, ", "),
		Detail:      "search reads code and docs; session_search reads this session's folded history; the memory tool keeps the notes"},
		On: h.cfg.Rag != nil}
}

// ragUp is whether dund answers a search now: one word, by text, one hit.
func (h *Harness) ragUp() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := h.cfg.Rag.Search(ctx, proto.RagSearch{Project: mcpserver.RaglitProject(h.cfg.Workspace), Index: memory.NotesIndex,
		Query: "dun", Limit: 1, Mode: "fts"})
	return !errors.Is(err, ErrRagDown) && ctx.Err() == nil
}

// setIndexAttach is a switch on an index at scope: on, off, or unset (nil).
func (h *Harness) setIndexAttach(name, scope string, on *bool) (string, error) {
	if name == projectIndexName {
		return "", errors.New("the project index (dund) is always on: it has no switch (DUN_MEMORY=0 runs a session without it)")
	}
	if name == mcpserver.ServerDocs { // the provider row: /mcp's switch on raglit
		return h.SetServerOn(context.Background(), mcpserver.ServerDocs, scope, on)
	}
	got, _, changed, err := h.SetIndex(context.Background(), mcpserver.ServerDocs, name, scope, on)
	if err != nil {
		return "", err
	}
	verb := "unset"
	if on != nil {
		verb = map[bool]string{true: "on", false: "off"}[*on]
	}
	if !changed {
		return fmt.Sprintf("%s was already %s for %s", got, verb, scopeWords(scope)), nil
	}
	return fmt.Sprintf("%s %s for %s", got, verb, scopeWords(scope)), nil
}
