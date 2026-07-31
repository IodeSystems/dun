package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

// Bubble Tea calls View() after EVERY message, so its cost is paid per token
// too — pacing refresh() does nothing for it. Measured separately because it is
// a different loop.
func BenchmarkView(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			m := buildModel(n, 8)
			m.refresh()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// One token message end to end, the way the runtime actually drives it:
// Update (which no longer renders) followed by the View the runtime calls.
func BenchmarkTokenMessageRoundTrip(b *testing.B) {
	m := buildModel(200, 8)
	m.refresh()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nm, _ := m.Update(evMsg{"type": "token", "text": "word "})
		m = nm.(tuiModel)
		_ = m.View()
	}
}

// dun must notice its own stutter: the person hitting it is working, not
// watching a metric. One warning per session, after enough samples to mean
// something, naming where the numbers are.
func TestSlowFrameDetection(t *testing.T) {
	var f frameStats
	// Fast frames never warn, however many.
	for i := 0; i < 500; i++ {
		if f.observe(stageUpdate, 200*time.Microsecond) {
			t.Fatal("fast frames must not warn")
		}
	}
	// A stuttering run does, exactly once.
	warns := 0
	for i := 0; i < 500; i++ {
		if f.observe(stageUpdate, 40*time.Millisecond) {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("want exactly one warning, got %d", warns)
	}
	rep := f.report()
	for _, want := range []string{"update", "slow", "p95"} {
		if !strings.Contains(rep, want) {
			t.Errorf("/perf report missing %q:\n%s", want, rep)
		}
	}
	// The warning has to say where to look.
	if w := f.slowWarning(); !strings.Contains(w, "/perf") {
		t.Errorf("warning should point at the numbers: %q", w)
	}
}

// Stages are reported separately because they failed separately: pacing fixed
// refresh() while View kept costing per message.
func TestPerfReport_SeparatesStages(t *testing.T) {
	var f frameStats
	f.observe(stageUpdate, time.Millisecond)
	f.observe(stageView, 2*time.Millisecond)
	f.observe(stageRefresh, 3*time.Millisecond)
	rep := f.report()
	for _, s := range []string{"update", "view", "refresh"} {
		if !strings.Contains(rep, s) {
			t.Errorf("report is missing the %s stage:\n%s", s, rep)
		}
	}
}
