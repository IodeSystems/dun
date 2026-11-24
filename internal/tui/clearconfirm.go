package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/textfmt"
)

// /clear with background jobs running, or open asks or plan items (USER
// 2026-11-24: "/clear should confirm with the user about background
// operations if running: stop, keep (new agent will be notified about
// them) — same with plan items"). The engine decides there is something to
// ask about and lists it (clear_confirm, cmd/dun/clear.go); the TUI shows
// the list, asks one category at a time on the status line, as the other
// one-key questions (jobkill.go), and sends the answer back as the slash
// line "clear jobs=… plan=…". Any other key cancels: nothing is sent and
// nothing is cleared. A turn running runs on while the question is open.

// clearListMax is how many jobs, and how many items, the list shows.
const clearListMax = 5

// clearConfirm is the clear_confirm event: show what outlives the
// conversation, then ask. A replayed one (an attach after the fact) is
// not asked again.
func (m *tuiModel) clearConfirm(ev evMsg) {
	if !freshEvent(ev) {
		return
	}
	jobs, _ := ev["jobs"].([]any)
	items, _ := ev["plan"].([]any)
	if len(jobs) == 0 && len(items) == 0 {
		return
	}
	m.append(StDim.Render("/clear: these outlive the conversation —"))
	now := time.Now()
	for i, j := range jobs {
		if i == clearListMax {
			m.append(StDim.Render(fmt.Sprintf("  +%d more jobs", len(jobs)-i)))
			break
		}
		jm, _ := j.(map[string]any)
		m.append(StDim.Render(fmt.Sprintf("  job #%d %s (%s)", int(evNum(jm["id"])),
			harness.ShortCommand(str(jm["command"]), 60), harness.ShortAge(now.Sub(time.Unix(int64(evNum(jm["started"])), 0))))))
	}
	for i, it := range items {
		if i == clearListMax {
			m.append(StDim.Render(fmt.Sprintf("  +%d more asks and plan items", len(items)-i)))
			break
		}
		im, _ := it.(map[string]any)
		m.append(StDim.Render("  " + harness.ClearItemLine(harness.ClearItem{Kind: str(im["kind"]), ID: str(im["id"]), Title: str(im["title"])})))
	}
	m.append(StDim.Render("  keep: the new conversation is told about them"))
	var answer []string
	send := func(m *tuiModel) {
		if !m.proc.controlCmd("slash", strings.TrimSpace("clear "+strings.Join(answer, " "))) {
			m.append(StErr.Render("no engine right now — /reconnect first"))
		}
	}
	askPlan := func(m *tuiModel) {
		if len(items) == 0 {
			send(m)
			return
		}
		pick := func(v string) func(m *tuiModel) {
			return func(m *tuiModel) { answer = append(answer, "plan="+v); send(m) }
		}
		m.askChoice("/clear: "+textfmt.Plural(len(items), "open ask/plan item")+" — d drop · k keep · esc cancel",
			map[string]func(*tuiModel){"d": pick("drop"), "k": pick("keep")})
	}
	if len(jobs) == 0 {
		askPlan(m)
		return
	}
	pick := func(v string) func(m *tuiModel) {
		return func(m *tuiModel) { answer = append(answer, "jobs="+v); askPlan(m) }
	}
	m.askChoice("/clear: "+textfmt.Plural(len(jobs), "background job")+" running — s stop · k keep · esc cancel",
		map[string]func(*tuiModel){"s": pick("stop"), "k": pick("keep")})
}
