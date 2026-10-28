package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The "indexes" switch kind (USER 2026-10-27, /rag 2026-10-28): /rag is the
// switch screen of every index raglit has, of every project, searchable,
// with each one on, off or unset at the global, project and session scopes;
// the nearest scope that says decides (attach.go). Unset everywhere, this
// project's own indexes are on and every other one off. The list is
// raglit's `index --list --json`; indexes a scope names that it does not
// list are shown too.

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
var listIndexes = func(ctx context.Context, cli, dir string) ([]raglitIndexRow, error) {
	p, err := FindCommand(cli)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "index", "--list", "--json")
	cmd.Dir = dir
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
	s, err := h.knownServer(ServerDocs)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	own := h.ownProject(s)
	// The provider first (USER 2026-10-28): raglit itself, its switch the
	// same as /mcp's, the indexes under it.
	provider := SwitchState{SwitchItem: SwitchItem{Name: ServerDocs, Description: h.describe(ServerDocs)},
		On: s.Autostart, From: s.Scopes.OnFrom,
		Global: onOffWord(s.Scopes.GlobalOn(s.Via)), Project: tri(s.Scopes.Project), Session: tri(s.Scopes.Session)}
	v.Items = append(v.Items, provider)
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
	mark(sc.AttachGlobal, ScopeGlobal, "on")
	mark(sc.DetachGlobal, ScopeGlobal, "off")
	mark(sc.AttachProject, ScopeProject, "on")
	mark(sc.DetachProject, ScopeProject, "off")
	mark(sc.AttachSession, ScopeSession, "on")
	mark(sc.DetachSession, ScopeSession, "off")
	seen := map[string]bool{}
	add := func(it SwitchItem) {
		seen[it.Name] = true
		m := scopes[it.Name]
		it.Parent = ServerDocs
		st := SwitchState{SwitchItem: it, Global: m[ScopeGlobal], Project: m[ScopeProject], Session: m[ScopeSession]}
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
	if e, ok := h.extension(ServerDocs); ok && e.Roles.Index != nil && e.Roles.Index.CLI != "" {
		cli = e.Roles.Index.CLI
	}
	rows, err := listIndexes(context.Background(), cli, h.cfg.Workspace)
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
	state := "off"
	switch {
	case h.mgr.ServerStarted(ServerDocs):
		state = "running"
	case s.Autostart:
		state = "on, not running"
	}
	v.Items[0].Source = fmt.Sprintf("rag provider · %s · %d indexes", state, len(v.Items)-1)
	on, from := h.WorkspaceIndexed()
	if from == "" {
		from = "the default"
	}
	ws := "this workspace is read into " + own + "'s index (" + from + ") — /rag workspace off to stop"
	if !on {
		ws = "this workspace is not indexed (" + from + ") — search reads the indexes switched on"
	}
	v.Status = []string{ws, "the nearest scope that switches an index decides; unset, this project's are on · ctrl+g/p/s on → off → unset"}
	return v
}

// setIndexAttach is a switch on an index at scope: on, off, or unset (nil).
func (h *Harness) setIndexAttach(name, scope string, on *bool) (string, error) {
	if name == ServerDocs { // the provider row: /mcp's switch on raglit
		return h.SetServerOn(context.Background(), ServerDocs, scope, on)
	}
	got, _, changed, err := h.SetIndex(context.Background(), ServerDocs, name, scope, on)
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
