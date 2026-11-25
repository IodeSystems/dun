package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/internal/repairs"
)

// sampleRepairs is a month's report as the reader builds it.
func sampleRepairs() repairs.Report {
	now := time.Now()
	return repairs.Report{
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local),
		Groups: []repairs.Group{
			{Kind: "repair empty-reply", Count: repairs.Count{N: 7, Today: 2, OK: 5, Failed: 2}, ByModel: map[string]repairs.Count{
				"openrouter · gemini-4": {N: 6, Today: 2, OK: 5, Failed: 1}, "local · qwen": {N: 1, Failed: 1}}},
			{Kind: "retry request 429", Count: repairs.Count{N: 3, WaitMS: 95000}, ByModel: map[string]repairs.Count{"openrouter · gemini-4": {N: 3, WaitMS: 95000}}},
			{Kind: "cancelled by user", Count: repairs.Count{N: 4, Today: 1}, ByModel: map[string]repairs.Count{"local · qwen": {N: 4, Today: 1}}},
		},
		Cost:  repairs.Cost{Rounds: 9, Tokens: 412000, Cost: 0.83, Currency: "USD", WaitMS: 95000},
		Tools: []repairs.Tool{{Tool: "node_edit", N: 3, ByCause: map[string]int{"warned": 2, "error": 1}}},
		Latency: []repairs.Latency{{Model: "gemini-4", Rounds: 120, LatencyP50: 2100, LatencyP95: 9400},
			{Model: "local-qwen", Rounds: 80, LatencyP50: 7100, LatencyP95: 30000, Timed: 80, PrefillP50: 1800, PrefillP95: 2200, GenerateP50: 82, GenerateP95: 106}},
		Cmds: []repairs.Cmd{{Cmd: "/mcp", Surface: "tui", N: 4, P50: 640, P95: 1900, Max: 1900, Last: 700},
			{Cmd: "/mcp", Surface: "engine", N: 4, P50: 600, P95: 1800, Max: 1800, Last: 650}},
	}
}

// /usage repairs opens the Repairs tab; the ledger read is asked for off
// the UI's goroutine, and its report redraws the tab.
func TestRepairsTab(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	m = nm.(tuiModel)
	m.runSlash("/usage repairs")
	v := m.statsPaging()
	if v == nil || v.tab != statsRepairs {
		t.Fatal("/usage repairs: not on the Repairs tab")
	}
	if !m.repairsWant || !strings.Contains(v.text[statsRepairs], "reading the stats ledger") {
		t.Fatalf("no read asked for: want %v text %q", m.repairsWant, v.text[statsRepairs])
	}
	nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = nm.(tuiModel)
	if cmd == nil || !m.repairsReading {
		t.Fatal("Update did not start the read")
	}
	nm, _ = m.Update(repairsMsg{sampleRepairs()})
	m = nm.(tuiModel)
	text := ansi.Strip(m.statsPaging().text[statsRepairs])
	for _, want := range []string{"cost of repairs", "412.0k", "$0.83", "1m35s", "repair empty-reply", "openrouter · gemini-4",
		"local · qwen", "cancelled by user", "tool errors", "node_edit", "warned 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("Repairs tab lacks %q:\n%s", want, text)
		}
	}
	if s := ansi.Strip(m.drawView()); !strings.Contains(s, "▸ Repairs") {
		t.Errorf("tab strip:\n%s", s)
	}
	t.Log("\n" + text)
	// An empty month says so.
	nm, _ = m.Update(repairsMsg{repairs.Report{Since: time.Now()}})
	m = nm.(tuiModel)
	if text := m.statsPaging().text[statsRepairs]; strings.Count(ansi.Strip(text), "none recorded this month") != 2 {
		t.Errorf("empty month:\n%s", text)
	}
}

// /usage perf opens the Perf tab: this session's frames and commands, the
// month's commands and round latency, a slow command flagged.
func TestPerfTab(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	m = nm.(tuiModel)
	m.drawView()
	m.runSlash("/usage perf")
	m.drawView() // the frame that shows it: /usage is timed
	nm, _ = m.Update(repairsMsg{sampleRepairs()})
	m = nm.(tuiModel)
	v := m.statsPaging()
	if v == nil || v.tab != statsPerf {
		t.Fatal("/usage perf: not on the Perf tab")
	}
	text := ansi.Strip(v.text[statsPerf])
	for _, want := range []string{"frames", "update", "view", "commands", "/usage", "commands this month", "1.9s!", "engine",
		"round latency by model", "local-qwen", "1.8k/s", "82/s"} {
		if !strings.Contains(text, want) {
			t.Errorf("Perf tab lacks %q:\n%s", want, text)
		}
	}
	t.Log("\n" + text)
	nm, _ = m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m = nm.(tuiModel)
	if v := m.statsPaging(); v == nil || v.tab != statsRepairs {
		t.Error("r did not move to Repairs")
	}
}

// A command the TUI answers is done at the next frame; one that sent to the
// engine waits for an answering event, and an ambient one does not answer.
func TestCmdTimer(t *testing.T) {
	var filed []string
	c := &cmdTimer{file: func(cmd string, ms int64) { filed = append(filed, cmd) }}
	c.update(time.Now().Add(-5 * time.Millisecond))
	c.begin("/help")
	c.ran()
	c.frame(time.Now())
	c.update(time.Now())
	c.begin("/mcp")
	engineSends.Add(1)
	c.ran()
	c.frame(time.Now())
	c.event("token")
	c.frame(time.Now())
	if len(filed) != 1 {
		t.Fatalf("filed %v before the engine answered", filed)
	}
	c.event("server")
	c.frame(time.Now())
	if len(filed) != 2 || filed[1] != "/mcp" {
		t.Fatalf("filed %v", filed)
	}
	s := c.session()
	if len(s) != 2 || s[0].Cmd != "/help" || s[0].P50 < 5 || s[0].Surface != "tui" {
		t.Errorf("session %+v", s)
	}
}
