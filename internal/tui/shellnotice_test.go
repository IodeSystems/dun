package tui

import (
	"fmt"
	"slices"
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
	e.state = viewExpanded
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

// Whatever a notice carries, it is one row until it is opened.
func TestNotice_FoldedToOneRow(t *testing.T) {
	big := "background job #6 succeeded — `go test ./...` (3m2s)\nlog: /tmp/j6.log\n" + strings.Repeat("ok  pkg\n", 400)
	for _, text := range []string{big, jobNotice(heredocCmd), jobNotice("make test")} {
		for _, w := range []int{100, 30} {
			e := noticeEntry(text, w)
			if rows := strings.Split(e.view(), "\n"); len(rows) != 1 {
				t.Fatalf("w=%d: %d rows folded", w, len(rows))
			} else if ansi.StringWidth(rows[0]) > w {
				t.Errorf("w=%d: wider than the pane: %q", w, ansi.Strip(rows[0]))
			}
			if !e.expandable() {
				t.Errorf("w=%d: nothing to open", w)
			}
		}
	}
	line := ansi.Strip(noticeEntry(big, 100).view())
	if !strings.Contains(line, "job #6 succeeded (3m2s) · $ go test ./...") {
		t.Errorf("line %q", line)
	}
	if got := ansi.Strip(noticeEntry(jobNotice(heredocCmd), 200).view()); !strings.Contains(got, " lines)") {
		t.Errorf("a multi-line command should say so: %q", got)
	}
}

// Opened, the output keeps its lines.
func TestNotice_OpensWithItsLines(t *testing.T) {
	rows := noticeRows(t, 100, "background job #6 failed — `make test` (2s)\nlog: /tmp/j.log\nfirst\nsecond")
	all := strings.Join(rows, "\n")
	for _, want := range []string{"make test", "log: /tmp/j.log", "first", "second"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in:\n%s", want, all)
		}
	}
	if !slices.Contains(rows, "first") {
		t.Errorf("output flattened:\n%s", all)
	}
}

// A notice that isn't a job's folds too, and one that fits has nothing to open.
func TestNotice_Plain(t *testing.T) {
	if e := noticeEntry("[para] proposal p1 decided", 100); e.expandable() {
		t.Error("a short notice should not open")
	}
	e := noticeEntry("[para] one\ntwo", 100)
	if !e.expandable() || strings.Contains(e.view(), "two") {
		t.Errorf("view %q", e.view())
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
