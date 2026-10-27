package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// ↑ with messages queued asks the engine for them back; its answer puts
// them in the composer, merged, and takes their "(queued)" lines out.
func TestUnqueue_UpLoadsThemIntoTheComposer(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "user", "content": "fix the typo", "queued": true})
	m = m.handleEvent(evMsg{"type": "queued", "count": float64(1)})
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = nm.(tuiModel)
	if !strings.Contains(rec.String(), `"unqueue"`) {
		t.Fatalf("up did not ask: %s", rec.String())
	}
	m = m.handleEvent(evMsg{"type": "control", "id": "unqueue", "action": `["fix the typo"]`})
	if got := m.input.Value(); got != "fix the typo" {
		t.Errorf("composer: %q", got)
	}
	for _, e := range m.convo {
		if e.provisional {
			t.Error("the (queued) line is still there")
		}
	}
}
