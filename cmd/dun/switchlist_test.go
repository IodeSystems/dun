package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The skills list: it opens when the engine answers, filters as you type,
// space and ctrl+p send the command a person could type for the row under
// the filter, tab cycles the session mode, and an answer refreshes it in
// place with the filter kept.
func TestSwitchList_Skills(t *testing.T) {
	v := newVtui(140, 30)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	items := []any{}
	for i := range 60 {
		items = append(items, map[string]any{"name": fmt.Sprintf("tool-%02d", i), "description": "does a thing", "source": "user",
			"tokens": float64(25), "on": true})
	}
	items = append(items, map[string]any{"name": "pdf-fill", "description": "fill PDF forms", "source": "user",
		"tokens": float64(40), "on": false, "from": "global", "global": "off"})
	ev := map[string]any{"type": "switches", "kind": "skills", "mode": "listed", "items": items,
		"about": map[string]any{"listed": "in every request", "surfaced": "offered when a turn fits"}}
	v.m.openSwitches("skills")
	if !strings.Contains(rec.String(), `"switches"`) {
		t.Fatalf("not asked: %s", rec.String())
	}
	v.event(ev)
	if v.m.switchesPaging() == nil {
		t.Fatal("the list did not open")
	}
	if got := v.view(); !strings.Contains(got, "skills · 60 of 61 on · listed · ~1.5k tokens in every request") {
		t.Errorf("title:\n%s", got)
	}
	v.m = typeStr(v.m, "pdf")
	got := v.view()
	if !strings.Contains(got, `filter "pdf": 1 shown`) || !strings.Contains(got, "○ pdf-fill") || strings.Contains(got, "tool-01") {
		t.Errorf("filtered:\n%s", got)
	}
	rec.Reset()
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	v.m = key(v.m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	for _, want := range []string{"skills on pdf-fill --session", "skills on pdf-fill --project", "skills mode listed --session"} {
		if !strings.Contains(rec.String(), want) {
			t.Errorf("sent %q, lacks %q", rec.String(), want)
		}
	}
	items[60] = map[string]any{"name": "pdf-fill", "description": "fill PDF forms", "source": "user", "tokens": float64(40),
		"on": true, "from": "session", "global": "off", "session": "on"}
	ev["message"] = "skills pdf-fill: on for this session (1 skill)"
	v.event(ev)
	sv := v.m.switchesPaging()
	if sv == nil || sv.query != "pdf" || !sv.rows[60].on {
		t.Fatalf("refresh: %+v", sv)
	}
	if !strings.Contains(lastRow(v.m), "on for this session") {
		t.Errorf("the answer is not flashed: %q", lastRow(v.m))
	}
}
