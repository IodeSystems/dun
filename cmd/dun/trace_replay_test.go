package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Replay the trace file's layout dump + scroll events against a synthetic
// conversation that matches the real rowOffset/blockH values.
func TestVtui_TraceReplaySynthetic(t *testing.T) {
	traceFile := os.Getenv("TRACE_FILE")
	if traceFile == "" {
		traceFile = "trace.jsonl"
	}
	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Skipf("no trace file: %v (set TRACE_FILE env var)", err)
	}

	lines := strings.Split(string(data), "\n")

	// Parse layout and scroll events
	var w, h int
	type layoutEntry struct {
		idx, rowOffset, blockH int
		userText               string
	}
	var layout []layoutEntry
	var scrolls []struct {
		yoff   int
		pinned bool
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "resize ") {
			fmt.Sscanf(line, "resize w=%d h=%d", &w, &h)
		} else if strings.HasPrefix(line, "layout ") {
			var idx, rowOffset, bh int
			var userText string
			fmt.Sscanf(line, "layout %d userText=%q rowOffset=%d h=%d", &idx, &userText, &rowOffset, &bh)
			layout = append(layout, layoutEntry{idx, rowOffset, bh, userText})
		} else if strings.HasPrefix(line, "scroll ") {
			var yoff int
			var pinned bool
			fmt.Sscanf(line, "scroll yoff=%d pinned=%v", &yoff, &pinned)
			scrolls = append(scrolls, struct {
				yoff   int
				pinned bool
			}{yoff, pinned})
		}
	}

	if w == 0 {
		w, h = 80, 24
	}
	t.Logf("terminal: %dx%d, layout entries: %d, scrolls: %d", w, h, len(layout), len(scrolls))

	// Find user messages from layout
	var userMsgs []layoutEntry
	for _, e := range layout {
		if e.userText != "" {
			userMsgs = append(userMsgs, e)
		}
	}
	for _, u := range userMsgs {
		t.Logf("  user=%q rowOffset=%d h=%d bottom=%d", u.userText, u.rowOffset, u.blockH, u.rowOffset+u.blockH)
	}

	// Build a conversation with the right total height
	maxRow := 0
	for _, e := range layout {
		bottom := e.rowOffset + e.blockH
		if bottom > maxRow {
			maxRow = bottom
		}
	}
	t.Logf("total content height: ~%d rows", maxRow)

	v := newVtui(w, h)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build content to match the layout's rowOffsets.
	// For each user message, place it at the right rowOffset by filling
	// with dummy content before it.
	wideLine := func(n int) string {
		return strings.Repeat("x", 70) + fmt.Sprintf(" line %d\n", n)
	}

	prevRow := 1 // after "ready" line
	for _, u := range userMsgs {
		// Fill with dummy content up to the user message's rowOffset
		gap := u.rowOffset - prevRow
		if gap < 0 {
			gap = 0
		}
		for gap > 0 {
			v.event(map[string]any{"type": "token", "text": wideLine(0)})
			v.event(map[string]any{"type": "done"})
			gap--
		}
		v.send(u.userText)
		prevRow = u.rowOffset + u.blockH
	}
	// Fill remaining to reach maxRow
	for prevRow < maxRow {
		v.event(map[string]any{"type": "token", "text": wideLine(0)})
		v.event(map[string]any{"type": "done"})
		prevRow++
	}

	// Verify rowOffsets match
	m := v.model()
	for _, u := range userMsgs {
		if u.idx >= len(m.convo) {
			continue
		}
		e := m.convo[u.idx]
		if e.userText == "" {
			continue
		}
		h := 0
		if u.idx < len(m.blockH) {
			h = m.blockH[u.idx]
		}
		t.Logf("  verify [%d] user=%q rowOffset=%d(have %d) h=%d(have %d)",
			u.idx, u.userText, u.rowOffset, e.rowOffset, u.blockH, h)
	}

	// Replay scroll events
	var seen []string
	for _, s := range scrolls {
		v.setYOffset(s.yoff)
		v.setScrollPin(false)
		overlay := v.model().scrollOverlay()
		if overlay != "" {
			plain := stripANSI(overlay)
			if len(seen) == 0 || seen[len(seen)-1] != plain {
				seen = append(seen, fmt.Sprintf("yoff=%d %s", s.yoff, plain))
			}
		}
	}

	t.Logf("unique overlays (%d):", len(seen))
	for _, s := range seen {
		t.Logf("  %s", s)
	}

	// Check all user messages appeared in the overlay
	for _, u := range userMsgs {
		found := false
		for _, s := range seen {
			if strings.Contains(s, u.userText) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("never saw %q in overlay", u.userText)
		}
	}
}
