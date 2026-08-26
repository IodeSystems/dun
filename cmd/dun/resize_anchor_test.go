package main

import (
	"strings"
	"testing"
)

// resize_anchor_test.go covers what an on-screen keyboard does.
//
// On a phone the soft keyboard opening and closing is a HEIGHT change, and it
// fires constantly. YOffset is the TOP visible row, so leaving it alone across
// one pins the top and lets the bottom move by the height delta — a reader
// parked on the last few done messages watched them slide out from under the
// fold every time the keyboard appeared.

// bottomRow is the last line the conversation pane is currently showing.
func bottomRow(m tuiModel) string {
	vis := m.vp.visible()
	if len(vis) == 0 {
		return ""
	}
	return strings.TrimSpace(stripANSI(vis[len(vis)-1]))
}

// filled drives a real conversation in, the way the engine's events arrive —
// building the model by hand leaves the pane empty and every assertion below
// vacuous.
func filled(t *testing.T, w, h int) *vtui {
	t.Helper()
	v := newVtui(w, h)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	for i := 1; i <= 25; i++ {
		v.send("question " + itoaT(i))
		v.event(map[string]any{"type": "token", "text": "answer " + itoaT(i)})
		v.event(map[string]any{"type": "done"})
	}
	if len(v.model().vp.lines) <= h {
		t.Fatalf("the pane holds %d rows at height %d; there is nothing to scroll",
			len(v.model().vp.lines), h)
	}
	return v
}

func itoaT(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// The reported bug: scrolled somewhere deliberately, keyboard up, keyboard down.
func TestResize_KeyboardKeepsTheBottomRow(t *testing.T) {
	v := filled(t, 60, 24)
	v.setScrollPin(false)
	v.setYOffset(v.model().vp.maxYOffset() - 3) // parked near, not at, the bottom
	want := bottomRow(v.model())
	if want == "" {
		t.Fatal("no content to anchor on — the fixture built an empty pane")
	}

	v.resize(60, 12) // keyboard up
	if got := bottomRow(v.model()); got != want {
		t.Errorf("keyboard up moved the bottom row:\n  was %q\n  now %q", want, got)
	}
	v.resize(60, 24) // keyboard down
	if got := bottomRow(v.model()); got != want {
		t.Errorf("keyboard down moved the bottom row:\n  was %q\n  now %q", want, got)
	}
}

// Repeatedly, because that is how it is actually used.
func TestResize_RepeatedKeyboardTogglesDoNotDrift(t *testing.T) {
	v := filled(t, 60, 24)
	v.setScrollPin(false)
	v.setYOffset(v.model().vp.maxYOffset() - 5)
	want := bottomRow(v.model())
	if want == "" {
		t.Fatal("no content to anchor on")
	}

	for i := 0; i < 4; i++ {
		v.resize(60, 12)
		v.resize(60, 24)
	}
	if got := bottomRow(v.model()); got != want {
		t.Errorf("four keyboard toggles drifted the bottom row:\n  was %q\n  now %q", want, got)
	}
}

// A pinned reader still follows the bottom — anchoring must not strand them
// above the newest message.
func TestResize_PinnedReaderStillFollowsTheBottom(t *testing.T) {
	v := filled(t, 60, 24)
	v.setScrollPin(true)
	v.m.vp.GotoBottom()

	v.resize(60, 12)
	if !v.model().vp.AtBottom() {
		t.Error("a pinned reader must still be at the bottom after the keyboard opens")
	}
	v.resize(60, 24)
	if !v.model().vp.AtBottom() {
		t.Error("a pinned reader must still be at the bottom after it closes")
	}
}

// At the very top, there is nothing above to give back — the anchor must clamp
// rather than scroll into negative space.
func TestResize_AtTheTopClampsInsteadOfDrifting(t *testing.T) {
	v := filled(t, 60, 24)
	v.setScrollPin(false)
	v.setYOffset(0)

	v.resize(60, 12)
	if got := v.model().vp.YOffset; got < 0 {
		t.Errorf("YOffset = %d; it must not go negative", got)
	}
	v.resize(60, 24)
	if got := v.model().vp.YOffset; got < 0 {
		t.Errorf("YOffset = %d after growing back", got)
	}
}
