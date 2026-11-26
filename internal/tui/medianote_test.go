package tui

import (
	"strings"
	"testing"
)

// A media note (a read's image scaled, or not sent) is a dim line.
func TestMediaNote(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 160, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "notification", "kind": "media", "text": "scaled shot.png 4032→1568 px, 3.1 MiB→412 KiB (magick)"})
	if v := stripANSI(m.View().Content); !strings.Contains(v, "▣ scaled shot.png 4032→1568 px, 3.1 MiB→412 KiB (magick)") {
		t.Errorf("media line missing:\n%s", v)
	}
}
