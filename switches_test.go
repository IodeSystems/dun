package dun

import (
	"path/filepath"
	"strings"
	"testing"
)

// A kind's switches: on unless a scope says otherwise; session over project
// over global; an exact name over a glob, a longer glob over a shorter; the
// mode resolves the same way, to the default when no scope sets it.
func TestSwitches_Resolve(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h := &Harness{cfg: Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl")}}
	items := []SwitchItem{{Name: "pdf-fill"}, {Name: "pdf-merge"}, {Name: "deploy"}, {Name: "notes"}}
	changes := 0
	k := switchKind{name: "things", modes: []string{"listed", "surfaced"}, items: func() []SwitchItem { return items },
		changed: func() { changes++ }}
	off, on := false, true
	mustSet := func(p, sc string, v *bool) {
		t.Helper()
		if _, err := h.setSwitch(k, p, sc, v); err != nil {
			t.Fatal(err)
		}
	}
	mustSet("pdf-*", ScopeGlobal, &off)
	mustSet("pdf-merge", ScopeProject, &on)
	mustSet("deploy", ScopeGlobal, &off)
	mustSet("deploy", ScopeSession, &on)
	v := h.switchView(k)
	got := map[string]SwitchState{}
	for _, it := range v.Items {
		got[it.Name] = it
	}
	if s := got["pdf-fill"]; s.On || s.From != ScopeGlobal || s.Global != "off" {
		t.Errorf("pdf-fill: %+v", s)
	}
	if s := got["pdf-merge"]; !s.On || s.From != ScopeProject || s.Global != "off" || s.Project != "on" {
		t.Errorf("pdf-merge: %+v", s)
	}
	if s := got["deploy"]; !s.On || s.From != ScopeSession {
		t.Errorf("deploy: %+v", s)
	}
	if s := got["notes"]; !s.On || s.From != "" {
		t.Errorf("notes: %+v", s)
	}
	if v.Mode != "listed" || v.ModeFrom != "" {
		t.Errorf("default mode: %+v", v)
	}
	if _, err := h.setSwitchMode(k, "surfaced", ScopeProject); err != nil {
		t.Fatal(err)
	}
	if v := h.switchView(k); v.Mode != "surfaced" || v.ModeFrom != ScopeProject || v.Modes[ScopeProject] != "surfaced" {
		t.Errorf("mode: %+v", v)
	}
	if _, err := h.setSwitchMode(k, "loud", ScopeGlobal); err == nil {
		t.Error("an unknown mode must be refused")
	}
	if _, err := h.setSwitch(k, "nope-*", ScopeGlobal, &off); err == nil || !strings.Contains(err.Error(), "nothing matches") {
		t.Errorf("a switch that matches nothing: %v", err)
	}
	mustSet("deploy", ScopeSession, nil) // unset
	if s := h.switchView(k).Items[2]; s.On || s.From != ScopeGlobal {
		t.Errorf("after unset: %+v", s)
	}
	if changes != 6 {
		t.Errorf("changed ran %d times", changes)
	}
}
