package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const heredocCmd = "cd /tmp && cat > recon.mjs <<'EOF'\nimport { spawn } from 'node:child_process'\nconst CHROME='/Applications/Google Chrome.app'\nEOF\nnode recon.mjs"

func jobNotice(cmd string) string {
	return "background job #2 succeeded — `" + cmd + "` (3s)\nlog: /tmp/job2.log\nok"
}

func noticeRows(t *testing.T, w int, text string) []string {
	t.Helper()
	var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: 30})
	m := mm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "notification", "kind": "job", "text": text})
	e := m.convo[len(m.convo)-1]
	return strings.Split(ansi.Strip(e.view()), "\n")
}

func TestNotice_HeredocKeepsLines(t *testing.T) {
	for _, w := range []int{100, 48} {
		rows := noticeRows(t, w, jobNotice(heredocCmd))
		if len(rows) < 5 {
			t.Fatalf("w=%d: %d rows, want the command on its own lines:\n%s", w, len(rows), strings.Join(rows, "\n"))
		}
		all := strings.Join(rows, "\n")
		for _, want := range []string{"🔔 background job #2 succeeded", "cat > recon.mjs", "import { spawn }", "node recon.mjs", "(3s)"} {
			if !strings.Contains(all, want) {
				t.Errorf("w=%d: missing %q:\n%s", w, want, all)
			}
		}
		for _, r := range rows {
			if ansi.StringWidth(r) > w {
				t.Errorf("w=%d: row wider than the pane: %q", w, r)
			}
		}
		t.Logf("w=%d\n%s", w, all)
	}
}

func TestNotice_OneLineStaysOne(t *testing.T) {
	rows := noticeRows(t, 100, jobNotice("make test"))
	if len(rows) != 1 || !strings.Contains(rows[0], "`make test`") {
		t.Fatalf("rows: %q", rows)
	}
}

func TestNotice_LongCommandCapped(t *testing.T) {
	var ls []string
	for i := 0; i < 40; i++ {
		ls = append(ls, fmt.Sprintf("echo line%d", i))
	}
	rows := noticeRows(t, 100, jobNotice(strings.Join(ls, "\n")))
	all := strings.Join(rows, "\n")
	if !strings.Contains(all, "echo line11") || strings.Contains(all, "echo line12") || !strings.Contains(all, "… 28 more lines") {
		t.Fatalf("not capped:\n%s", all)
	}
	if len(rows) > shellMaxLines+4 {
		t.Fatalf("%d rows", len(rows))
	}
}
