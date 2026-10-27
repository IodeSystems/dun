package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The "indexes" switch kind (USER 2026-10-27): bare /raglit attach is the
// switch screen of every index raglit has, of every project, searchable,
// with each one's attachment at the global, project and session scopes. An
// index is attached where a scope says on (switching one off detaches it
// there; an attachment has no "off" of its own), so unlike the other kinds
// a row is off unless a scope attaches it. The list is raglit's
// `index --list --json`; attached indexes it does not list are shown too.

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
	return switchKind{name: indexesKind, slash: ServerDocs + " attach", view: h.indexesView, set: h.setIndexAttach}
}

func (h *Harness) indexesView() SwitchView {
	v := SwitchView{Kind: indexesKind, Slash: ServerDocs + " attach", Modes: map[string]string{}}
	at, err := h.Attachments(ServerDocs)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	scopes := map[string]map[string]string{}
	for _, a := range at {
		m := map[string]string{}
		for _, sc := range a.Scopes {
			m[sc] = "on"
		}
		scopes[a.Index] = m
	}
	own := raglitProject(h.cfg.Workspace)
	if s, err := h.knownServer(ServerDocs); err == nil {
		own = h.ownProject(s)
	}
	seen := map[string]bool{}
	add := func(it SwitchItem) {
		seen[it.Name] = true
		m := scopes[it.Name]
		st := SwitchState{SwitchItem: it, Global: m[ScopeGlobal], Project: m[ScopeProject], Session: m[ScopeSession]}
		for _, sc := range switchScopes {
			if m[sc] == "on" {
				st.On, st.From = true, sc
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
		v.Note = err.Error() + " — only the attached indexes are shown"
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
			det = append(det, "this project's own: search reads it already; attaching it changes nothing")
		}
		if r.Ambient {
			det = append(det, "ambient: every search reads it")
		}
		if !r.Meaning {
			det = append(det, "exact-word search only (no embeddings)")
		}
		add(SwitchItem{Name: name, Description: desc, Source: src, Detail: strings.Join(det, "\n")})
	}
	for _, a := range at {
		if !seen[a.Index] {
			add(SwitchItem{Name: a.Index, Source: "attached", Description: "not in raglit's list"})
		}
	}
	on, from := h.WorkspaceIndexed()
	if from == "" {
		from = "the default"
	}
	ws := "this workspace is read into " + own + "'s index (" + from + ") — /" + ServerDocs + " workspace off to stop"
	if !on {
		ws = "this workspace is not indexed (" + from + ") — search reads " + own + "'s own indexes and the attached ones"
	}
	v.Status = []string{ws, "an index attached at any scope is searched; ctrl+g/p/s attach or detach there"}
	return v
}

// setIndexAttach is a switch on an index: on attaches it at scope, off or
// unset detaches it there.
func (h *Harness) setIndexAttach(name, scope string, on *bool) (string, error) {
	attach := on != nil && *on
	got, _, changed, err := h.AttachIndex(context.Background(), ServerDocs, name, scope, attach)
	if err != nil {
		return "", err
	}
	verb := map[bool]string{true: "attached", false: "detached"}[attach]
	if !changed {
		return fmt.Sprintf("%s was already %s for %s", got, verb, scopeWords(scope)), nil
	}
	return fmt.Sprintf("%s %s for %s", got, verb, scopeWords(scope)), nil
}
