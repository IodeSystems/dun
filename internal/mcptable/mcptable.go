// Package mcptable draws the server switches as a table, for the TUI and for
// /mcp as text.
package mcptable

import (
	"fmt"
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
)

// SwitchLegend reads the table's marks.
const SwitchLegend = "● on · ○ off · – unset · the session's switch wins, then the project's, then the global one"

// CellMark is a switch as a table cell.
func CellMark(v string) string {
	switch v {
	case "on":
		return "●"
	case "off":
		return "○"
	}
	return "–"
}

// CellWord is a switch as a word.
func CellWord(v string) string {
	if v == "" {
		return "unset"
	}
	return v
}

// StateCell is "on (project)": what the server is, and which switch said so.
func StateCell(st dun.ServerState) string {
	from := st.From
	if from == "flag" {
		from = "this run"
	}
	if from == "" {
		return textfmt.OnOff(st.Auto)
	}
	return textfmt.OnOff(st.Auto) + " (" + from + ")"
}

// ViaCell is "scripted (global)": the mode, and which switch said so.
func ViaCell(st dun.ServerState) string {
	if strings.HasPrefix(st.ID, dun.BuiltinPrefix) {
		return "dun's own tools"
	}
	if st.Via == "" {
		return "standalone (built-in)"
	}
	return dun.ModeWord(st.Via) + " (" + st.ViaFrom + ")"
}

// InstanceCell is "shared (global)": how many connections, and which switch
// said so; a built-in's fixed one is marked built-in.
func InstanceCell(st dun.ServerState) string {
	if strings.HasPrefix(st.ID, dun.BuiltinPrefix) {
		return "dun's own tools"
	}
	inst := st.Instance
	if inst == "" {
		inst = dun.InstanceShared
	}
	if st.InstanceFrom == "" {
		return inst + " (built-in)"
	}
	return inst + " (" + st.InstanceFrom + ")"
}

// tableCols are the column headers after the id, with each one's hotkey
// (the TUI's /mcp view cycles a column with it). "" is no key.
var tableCols = []struct{ word, key string }{
	{"global", "g"}, {"project", "p"}, {"session", "s"}, {"mode", "m"}, {"instance", "i"}, {"→ state", ""},
}

// keyStyle renders a header word with its hotkey letter marked.
type keyStyle func(word, key string) string

// PlainKey marks nothing: the text listing every client gets.
func PlainKey(word, _ string) string { return word }

// Widths are the columns that size to their cells: the id, the mode and
// the instance.
type Widths struct{ ID, Via, Instance int }

// TableWidths are the widths states' rows need.
func TableWidths(states []dun.ServerState) Widths {
	w := Widths{ID: len("server"), Via: len("mode"), Instance: len("instance")}
	for _, st := range states {
		w.ID = max(w.ID, len(st.ID))
		w.Via = max(w.Via, len([]rune(ViaCell(st))))
		w.Instance = max(w.Instance, len([]rune(InstanceCell(st))))
	}
	return w
}

// ServerHeader is the table's header row.
func ServerHeader(cw Widths, key keyStyle) string {
	var b strings.Builder
	b.WriteString(textfmt.Pad("server", cw.ID) + "  " + textfmt.Pad("proto", protoW))
	for i, c := range tableCols {
		b.WriteString("  ")
		w := len([]rune(c.word))
		if i < 3 {
			w = max(w, 7)
		} else if c.word == "mode" {
			w = cw.Via
		} else if c.word == "instance" {
			w = cw.Instance
		}
		if c.key == "" {
			b.WriteString(c.word)
			continue
		}
		b.WriteString(key(c.word, c.key) + strings.Repeat(" ", max(0, w-len([]rune(c.word)))))
	}
	return b.String()
}

// ServerRow is one server's row: the switches, where it runs, what it is,
// and what it is doing.
func ServerRow(st dun.ServerState, cw Widths, detail string) string {
	cell := func(v string) string { return "   " + CellMark(v) + "   " }
	return textfmt.Pad(st.ID, cw.ID) + "  " + textfmt.Pad(st.Proto, protoW) + "  " + cell(st.Global) + "  " + cell(st.Project) + "  " + cell(st.Session) +
		"  " + textfmt.Pad(ViaCell(st), cw.Via) + "  " + textfmt.Pad(InstanceCell(st), cw.Instance) + "  " + StateCell(st) + " · " + detail
}

// ServerTable is the header and a row per server, indented.
func ServerTable(states []dun.ServerState, key keyStyle) string {
	cw := TableWidths(states)
	rows := []string{"  " + ServerHeader(cw, key)}
	for _, st := range states {
		rows = append(rows, "  "+ServerRow(st, cw, ServerDetail(st)))
	}
	return strings.Join(rows, "\n")
}

// protoW is the proto column: stdio, https, http.
const protoW = 5

// ToolCost is "160 tools · ~38k tokens · 12 hidden".
func ToolCost(st dun.ServerState) string {
	s := textfmt.Plural(st.Tools, "tool") + " · ~" + numfmt.KTokens(st.Cost) + " tokens"
	if st.Hidden > 0 {
		s += fmt.Sprintf(" · %d hidden", st.Hidden)
	}
	return s
}

// ServerDetail is what a server is doing now, after its state column.
func ServerDetail(st dun.ServerState) string {
	var s string
	switch {
	case st.Via == dun.ViaShell && st.Auto:
		s = HostedSummary(st)
	case st.Via == dun.ViaShell:
		s = "not hosted"
	case st.Starting:
		s = "starting…"
	case st.Running && st.Index == dun.IndexBuilding:
		s = fmt.Sprintf("running · indexing %ds · ", int(st.IndexSeconds+0.5)) + ToolCost(st)
	case st.Running && st.Index == dun.IndexFailed:
		s = "running · index failed: " + textfmt.OneLine(st.IndexErr) + " · " + ToolCost(st)
	case st.Running:
		s = "running · " + ToolCost(st)
	case st.Err != "":
		s = "stopped · last error: " + textfmt.OneLine(st.Err)
	default:
		s = "stopped"
	}
	if st.Auth != "" {
		s += " · " + st.Auth
	}
	return s
}

// HostedSummary is a shell-placed server's state: where the model finds it,
// and whether the running shell connected it.
func HostedSummary(st dun.ServerState) string {
	s := fmt.Sprintf("scripted as %s (eval: help(%q))", st.Namespace, st.Namespace)
	switch {
	case st.Hosted:
		return s + fmt.Sprintf(" · %d tools · 0 tokens per request (its schemas stay in the shell)", st.Tools)
	case st.Err != "":
		return s + " · not connected: " + textfmt.OneLine(st.Err)
	}
	return s
}
