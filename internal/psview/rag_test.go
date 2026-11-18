package psview

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
	"github.com/iodesystems/dun/proto"
)

// ragSetup is setup's reading with dund's row (pid 90) carrying retrieval:
// the session's project dun ingesting its code, an index no session owns,
// two idle indexes.
func ragSetup(t *testing.T, w int) *model {
	t.Helper()
	f := newFixture()
	sample := func() (dundcli.PSView, procps.Snapshot, error) {
		v, snap, err := f.sample()
		var rows []procps.Row
		for _, g := range v.Groups {
			rows = append(rows, g.Rows...)
		}
		dund := procps.Row{PID: 90, PPID: 1, Group: procps.GroupService, Session: procps.GroupService, Job: "dund",
			Argv: "dun dund serve", CPU: 40, RSS: 300 << 20, ReadBps: 0, WritBps: 4096, AgeSec: 7200}
		rows = append(rows, dund)
		list := &proto.ProcsList{DundPID: 90, Sessions: []proto.ProcSession{
			{ID: sesID, Workspace: "/home/u/src/dun", EnginePID: 100, Project: "dun"}},
			Rag: &proto.ProcRag{
				Embed: proto.ProcRagEmbed{Model: "nomic-embed-text", Where: "local", InFlight: 1, Width: 4},
				Indexes: []proto.ProcRagIndex{
					{Name: "dun/code", Project: "dun", Kind: "code", State: "embedding", Queue: 40, Running: 2,
						EmbedInFlight: 1, FilesMin: 30, ChunksMin: 210, EmbedsMin: 8, EmbedMsMin: 4200, TokensMin: 9000,
						Files: 400, Chunks: 3000, Embeds: 90, EmbedMs: 61000, Tokens: 120000, IdleSec: 0,
						LastErr: "embed: 503 overloaded", LastErrAt: f.at.Add(-2 * time.Minute)},
					{Name: "other/docs", Project: "other", Kind: "docs", State: "ingesting", Queue: 3, IdleSec: 4},
					{Name: "b/code", Project: "b", Kind: "code", State: "idle", IdleSec: 7200},
					{Name: "global/memories", Kind: "memories", State: "idle", IdleSec: -1},
				},
			}}
		procps.AttachRag(rows, 90, list.Rag, list.Sessions, f.at)
		v.Groups, v.List = procps.Groups(rows), list
		return v, snap, err
	}
	m := newModel(sample, Actions{}, 2*time.Second, func(int) string { return "" })
	m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
	v, s, err := sample()
	m.Update(sampleMsg{v: v, snap: s, err: err})
	return m
}

func TestRagRows(t *testing.T) {
	for _, w := range []int{60, 100} {
		m := ragSetup(t, w)
		r := m.render()
		for _, want := range []string{"rag embed nomic-embed-text", "↳ 1021-2228 dun  rag dun code", "↳ (none)  rag other",
			"2 idle indexes"} {
			if !strings.Contains(r, want) {
				t.Errorf("w %d: lacks %q:\n%s", w, want, r)
			}
		}
		if w == 100 && !strings.Contains(r, "rag dun code  embedding  queue 40  files/min 30  embeds/min 8") {
			t.Errorf("w 100: the index line:\n%s", r)
		}
		for i, l := range strings.Split(r, "\n") {
			if lw := lipgloss.Width(l); lw > w {
				t.Errorf("w %d line %d is %d wide: %q", w, i, lw, l)
			}
		}
		// The rag lines are under dund's row, in the (service) group.
		var kinds []itemKind
		for _, it := range m.items() {
			if it.group.Group == procps.GroupService {
				kinds = append(kinds, it.kind)
			}
		}
		if want := []itemKind{kGroup, kProc, kRagEmbed, kRag, kRag, kRagIdle}; !equalKinds(kinds, want) {
			t.Errorf("w %d: the (service) group's lines %v, want %v", w, kinds, want)
		}

		// An index's line is selectable; enter shows its counters.
		found := false
		for i, it := range m.items() {
			if it.kind == kRag && it.rag.Name == "dun/code" {
				m.moveTo(i)
				found = true
			}
		}
		if it, _ := m.selected(); !found || it.kind != kRag {
			t.Fatalf("w %d: the index is not selected: %+v", w, m.sel)
		}
		press(m, "enter")
		r = m.render()
		for _, want := range []string{"─ rag dun/code", "queue     40 pending  2 running  1 embeds in flight",
			"last min  30 files  210 chunks  8 embeds  4.2s embedding",
			"total     400 files", "last err  2m ago  embed: 503"} {
			if !strings.Contains(r, want) {
				t.Errorf("w %d: details lack %q:\n%s", w, want, r)
			}
		}
		for i, l := range strings.Split(r, "\n") {
			if lw := lipgloss.Width(l); lw > w {
				t.Errorf("w %d details line %d is %d wide: %q", w, i, lw, l)
			}
		}
		// The selection stays on the index across readings; k refuses it.
		refreshRag(m)
		if it, _ := m.selected(); it.kind != kRag || it.rag.Name != "dun/code" {
			t.Errorf("w %d: selection after a reading: %+v", w, m.sel)
		}
		press(m, "esc", "k")
		if m.ask != nil || !m.statusErr {
			t.Errorf("w %d: k on an index: ask %v status %q", w, m.ask, m.status)
		}
	}
}

func refreshRag(m *model) {
	v, s, err := m.sample()
	m.Update(sampleMsg{v: v, snap: s, err: err})
}

func equalKinds(a, b []itemKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
