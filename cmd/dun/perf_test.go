package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// buildModel makes a TUI with a realistic conversation: n finalized blocks of
// roughly the size a tool result or a reply actually is.
func buildModel(n, blockLines int) tuiModel {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(tuiModel)
	body := strings.Repeat("some conversation content that wraps across the terminal width\n", blockLines)
	for i := 0; i < n; i++ {
		m.convo = append(m.convo, convoEntry{collapsed: fmt.Sprintf("block %d\n%s", i, body)})
	}
	return m
}

// refresh() re-wraps EVERY block, and the token handler calls it once per
// streamed token. This benchmark is the cost of one keystroke-blocking frame.
func BenchmarkRefresh(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			m := buildModel(n, 8)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.refresh()
			}
		})
	}
}

// A 100-token reply as it actually costs now: every token handled, and frames
// paced by renderHz rather than one per token. Before pacing + the wrap cache
// this was 973ms at 200 blocks — a full second of CPU on the goroutine that
// also reads the keyboard.
func BenchmarkStreamingTurn(b *testing.B) {
	// 100 tokens arriving over ~1s at 30Hz is ~30 frames; at a realistic
	// streaming rate it is far fewer. Three frames is the conservative floor.
	const tokens, frames = 100, 3
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			m := buildModel(n, 8)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.cur = ""
				for t := 0; t < tokens; t++ {
					m = m.handleEvent(evMsg{"type": "token", "text": "word "})
				}
				for f := 0; f < frames; f++ {
					m.refresh()
				}
			}
		})
	}
}

// The input path must stay cheap no matter how big the conversation is: this is
// what competes with a keystroke.
func BenchmarkTokenHandling(b *testing.B) {
	m := buildModel(200, 8)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m = m.handleEvent(evMsg{"type": "token", "text": "word "})
	}
}

// Pacing must not lose text. A token marks the screen dirty and a tick draws
// it; if the tick is ever not scheduled, streamed output simply never appears.
func TestRenderPacing_TokensReachTheScreen(t *testing.T) {
	m := buildModel(2, 2)
	nm, cmd := m.Update(evMsg{"type": "token", "text": "hello from the model"})
	m = nm.(tuiModel)
	if !m.renderDue {
		t.Fatal("a token should mark the screen dirty")
	}
	if !m.tickPending || cmd == nil {
		t.Fatal("a token must schedule the frame that draws it")
	}
	// A second token must NOT pile on another tick.
	nm, _ = m.Update(evMsg{"type": "token", "text": " and more"})
	m = nm.(tuiModel)
	if !m.tickPending {
		t.Fatal("tick lost")
	}
	// The tick draws everything accumulated since the last one.
	nm, _ = m.Update(renderTickMsg{})
	m = nm.(tuiModel)
	if m.renderDue {
		t.Error("the frame should have cleared the dirty flag")
	}
	if !strings.Contains(m.vp.View(), "hello from the model") {
		t.Errorf("streamed text never reached the viewport:\n%s", m.vp.View())
	}
}

// While a turn is running the clock keeps itself alive, so a frame is never
// waiting on the next token to be scheduled.
func TestRenderPacing_KeepsTickingWhileBusy(t *testing.T) {
	m := buildModel(2, 2)
	m.busy, m.renderDue, m.tickPending = true, true, true
	nm, cmd := m.Update(renderTickMsg{})
	m = nm.(tuiModel)
	if cmd == nil || !m.tickPending {
		t.Error("a busy turn should keep the render clock running")
	}
	// Idle: the clock stops rather than spinning forever.
	m.busy, m.renderDue, m.tickPending = false, true, true
	nm, cmd = m.Update(renderTickMsg{})
	m = nm.(tuiModel)
	if cmd != nil || m.tickPending {
		t.Error("an idle TUI must not keep scheduling frames")
	}
}
