package main

import (
	"fmt"
	"strings"
	"testing"
)

// Verify the scroll overlay picks the off-screen message closest to the
// viewport top (the one just scrolled past), not the last one in the array.
func TestVtui_ScrollOverlayPicksClosest(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// "first" at row ~1, bottom ~2
	v.send("first")
	// 50 lines of content: rows 2-51
	for i := 0; i < 50; i++ {
		v.event(map[string]any{"type": "token", "text": strings.Repeat("x", 70) + fmt.Sprintf(" a%d\n", i)})
		v.event(map[string]any{"type": "done"})
	}
	// "second" at row ~52, bottom ~53
	v.send("second")
	// 50 more lines: rows 53-102
	for i := 0; i < 50; i++ {
		v.event(map[string]any{"type": "token", "text": strings.Repeat("x", 70) + fmt.Sprintf(" b%d\n", i)})
		v.event(map[string]any{"type": "done"})
	}

	m := v.model()
	for i, e := range m.convo {
		if e.userText != "" {
			h := 0
			if i < len(m.blockH) {
				h = m.blockH[i]
			}
			t.Logf("[%d] user=%q rowOffset=%d h=%d bottom=%d", i, e.userText, e.rowOffset, h, e.rowOffset+h)
		}
	}

	// At yOff=60: both "first"(bottom=2) and "second"(bottom=53) are off-screen.
	// The closest to viewport top is "second"(53), so overlay should show "second".
	v.setYOffset(60)
	v.setScrollPin(false)
	overlay := stripANSI(v.model().scrollOverlay())
	if !strings.Contains(overlay, "second") {
		t.Errorf("yOff=60: expected 'second', got %q", overlay)
	}

	// At yOff=10: only "first"(bottom=2) is off-screen. "second"(bottom=53) is visible.
	// Overlay should show "first".
	v.setYOffset(10)
	v.setScrollPin(false)
	overlay = stripANSI(v.model().scrollOverlay())
	if !strings.Contains(overlay, "first") {
		t.Errorf("yOff=10: expected 'first', got %q", overlay)
	}

	// At yOff=1: nothing off-screen.
	v.setYOffset(1)
	v.setScrollPin(false)
	overlay = stripANSI(v.model().scrollOverlay())
	if overlay != "" {
		t.Errorf("yOff=1: expected no overlay, got %q", overlay)
	}
}
