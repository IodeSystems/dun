package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type recWC struct{ bytes.Buffer }

func (r *recWC) Close() error { return nil }

// A server with hundreds of tools: typing filters the list (name matches
// first, then descriptions), the title says how many are shown, space
// toggles the selected tool of the FILTERED list, and esc clears the filter
// before it closes the view.
func TestMCPTools_TypeToFilter(t *testing.T) {
	v := newVtui(120, 30)
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	tools := []any{}
	for i := range 120 {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("tool_%03d", i), "description": "does thing " + fmt.Sprint(i),
			"tokens": float64(300), "enabled": true, "schema": "{}"})
	}
	tools = append(tools,
		map[string]any{"name": "issue_create", "description": "open an issue", "tokens": float64(900), "enabled": true, "schema": "{}"},
		map[string]any{"name": "ticket_new", "description": "files a new issue in the tracker", "tokens": float64(700), "enabled": false, "schema": "{}"})
	v.m.openMCPTools("vay")
	v.event(map[string]any{"type": "mcp_tools", "server": "vay", "tools": tools})
	if v.m.toolsPaging() == nil {
		t.Fatal("the tools view did not open")
	}
	v.m = typeStr(v.m, "issue")
	tv := v.m.toolsPaging()
	if tv == nil || tv.query != "issue" {
		t.Fatalf("typing did not filter (letters such as s and g must not be keys here): %+v", tv)
	}
	if s := tv.shown(); len(s) != 2 || tv.rows[s[0]].name != "issue_create" || tv.rows[s[1]].name != "ticket_new" {
		t.Fatalf("shown: %v", s)
	}
	view := v.view()
	if !strings.Contains(view, `filter "issue": 2 shown`) || !strings.Contains(view, "issue_create") || strings.Contains(view, "tool_001") {
		t.Errorf("view:\n%s", view)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyDown})
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if got := rec.String(); !strings.Contains(got, "toggle vay ticket_new on") {
		t.Errorf("space toggled the wrong row: %q", got)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if tv := v.m.toolsPaging(); tv == nil || tv.query != "issu" {
		t.Errorf("backspace: %+v", tv)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if tv := v.m.toolsPaging(); tv == nil || tv.query != "" || len(tv.shown()) != 122 {
		t.Fatalf("esc should clear the filter first: %+v", tv)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.m.paging {
		t.Error("esc on an empty filter closes the view")
	}
}
