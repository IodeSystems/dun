package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Trace replay: take a layout+scroll recording made by `/trace on` and put the
// frames the user actually saw back on a virtual screen.
//
// The point of a replay is to be an INDEPENDENT witness. An earlier version of
// this test re-implemented scrollOverlay's "nearest off-screen user message"
// search inside the test and compared the two — which passes for any behaviour
// scrollOverlay has, including a scroll indicator that never reaches the screen
// at all. So this one asserts nothing about which message *should* win. It
// renders the real View, truncates it the way Bubble Tea's renderer does, reads
// the top row off the resulting screen, and checks two things that cannot be
// derived from the code under test:
//
//   - soundness: whatever the top row names must be a user message that really
//     is off the top of the viewport at that scroll position. (The bug this
//     replaces caught: the frame overdrew by a row, the renderer dropped the
//     header, and the top row was the static task line — which names the NEWEST
//     message, one still below the fold.)
//   - liveness: over a scroll range that crosses several messages, the top row
//     must not be constant.
//
// Which message wins where is specified by the synthetic tests in tui_test.go,
// against a layout those tests build themselves.

// trunc shortens a message for a log line or a failure message.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type traceLayout struct {
	idx       int
	kind      string
	userText  string
	rowOffset int
	blockH    int
}

type traceFile struct {
	w, h    int
	layout  []traceLayout
	scrolls []int
}

var (
	traceResizeRe = regexp.MustCompile(`^resize w=(\d+) h=(\d+)$`)
	traceLayoutRe = regexp.MustCompile(`^layout (\d+) kind=(\w+) userText=("(?:[^"\\]|\\.)*") rowOffset=(\d+) h=(\d+)$`)
	traceScrollRe = regexp.MustCompile(`^scroll yoff=(\d+) pinned=(\w+)$`)
)

func parseTrace(t *testing.T, data string) traceFile {
	t.Helper()
	tf := traceFile{w: 80, h: 24}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case traceResizeRe.MatchString(line):
			m := traceResizeRe.FindStringSubmatch(line)
			tf.w, _ = strconv.Atoi(m[1])
			tf.h, _ = strconv.Atoi(m[2])
		case traceLayoutRe.MatchString(line):
			m := traceLayoutRe.FindStringSubmatch(line)
			text, err := strconv.Unquote(m[3])
			if err != nil {
				t.Fatalf("layout line has an unparseable userText: %s", line)
			}
			e := traceLayout{kind: m[2], userText: text}
			e.idx, _ = strconv.Atoi(m[1])
			e.rowOffset, _ = strconv.Atoi(m[4])
			e.blockH, _ = strconv.Atoi(m[5])
			tf.layout = append(tf.layout, e)
		case traceScrollRe.MatchString(line):
			m := traceScrollRe.FindStringSubmatch(line)
			y, _ := strconv.Atoi(m[1])
			tf.scrolls = append(tf.scrolls, y)
		}
	}
	return tf
}

// install puts a recorded layout into a real model: convo entries at their
// recorded indices, their recorded row offsets and heights, and a viewport
// holding as many lines as the recording spanned. Nothing is re-rendered —
// the recording IS the layout, and reproducing it by synthesising content that
// happens to wrap to the same heights is what made the old replay unusable.
//
// A trace dumps entries, not model state, so the replay sets m.task the way the
// app does — from the newest user message. It no longer draws anything (it used
// to take the row under the scroll overlay and read as a broken copy of it), but
// keeping it set is what makes this a replay rather than an approximation.
func (tf traceFile) install(v *vtui) {
	maxIdx, maxRow := 0, 0
	for _, e := range tf.layout {
		maxIdx = max(maxIdx, e.idx)
		maxRow = max(maxRow, e.rowOffset+e.blockH)
	}
	m := &v.m
	m.convo = make([]convoEntry, maxIdx+1)
	// The frame is the single geometry truth refresh writes; the replay installs
	// a recorded layout straight into it (no refresh runs during replay). Both
	// blockH and rowOffset live in the frame now — the per-entry rowOffset on
	// convoEntry is the record the trace parses back, but the readers (the
	// scroll overlay in particular) read the frame, so it must be populated too.
	m.frame = frame{blockH: make([]int, maxIdx+1), rowOffset: make([]int, maxIdx+1)}
	for _, e := range tf.layout {
		m.convo[e.idx] = convoEntry{userText: e.userText, rowOffset: e.rowOffset}
		m.frame.blockH[e.idx] = e.blockH
		m.frame.rowOffset[e.idx] = e.rowOffset
		if e.userText != "" {
			m.task = e.userText // last user message wins, as replay() does
		}
	}
	m.vp.SetLines(make([]string, maxRow))
	m.contentGen++
}

// screen renders the model and truncates it the way Bubble Tea's standard
// renderer does — it keeps the LAST h lines, so a frame one row too tall loses
// its header before it is ever displayed (standard_renderer.go, flush).
func screen(v *vtui) []string {
	rows := strings.Split(v.view(), "\n")
	if len(rows) > v.h {
		rows = rows[len(rows)-v.h:]
	}
	return rows
}

func TestVtui_TraceReplay(t *testing.T) {
	path := os.Getenv("TRACE_FILE")
	if path == "" {
		// The recording from the session where the indicator was reported stuck.
		path = "testdata/scroll-stuck.jsonl"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no trace to replay: %v (set TRACE_FILE)", err)
	}
	tf := parseTrace(t, string(data))
	if len(tf.scrolls) == 0 {
		t.Skip("trace has no scroll events")
	}

	// Every user message in the recording, by the row its block ended on.
	type userMsg struct {
		text   string
		bottom int
	}
	var users []userMsg
	for _, e := range tf.layout {
		if e.userText != "" {
			users = append(users, userMsg{e.userText, e.rowOffset + e.blockH})
		}
	}
	if len(users) < 2 {
		t.Skipf("trace has %d user messages; need at least 2 to see the indicator move", len(users))
	}

	v := newVtui(tf.w, tf.h)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	tf.install(v)

	// offScreen reports whether a message that the top row could be naming was
	// genuinely above the fold at this scroll position.
	offScreen := func(text string, yOff int) (found, above bool) {
		for _, u := range users {
			// The overlay clips long messages to the width, so compare on the
			// prefix that survived.
			if strings.HasPrefix(u.text, text) || strings.HasPrefix(text, u.text) {
				return true, u.bottom <= yOff
			}
		}
		return false, false
	}

	var tops []string
	for _, yOff := range tf.scrolls {
		v.m.vp.SetYOffset(yOff)
		v.m.scrollPinned = false
		rows := screen(v)
		if len(rows) != tf.h {
			t.Fatalf("yoff=%d: frame is %d rows on a %d-row terminal", yOff, len(rows), tf.h)
		}
		top := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rows[0]), "›"))
		t.Logf("yoff=%4d top=%q", yOff, trunc(top, 60))
		if len(tops) == 0 || tops[len(tops)-1] != top {
			tops = append(tops, top)
		}
		if top == "" {
			continue
		}
		// Soundness. A top row naming nothing in the conversation is the header
		// or some other line — fine. A top row naming a user message that is
		// still on screen or below the fold is the indicator lying.
		if found, above := offScreen(top, yOff); found && !above {
			t.Errorf("yoff=%d: top row names %q, which is not off the top of the viewport",
				yOff, trunc(top, 60))
		}
	}

	// Liveness. Count the message boundaries the recorded scroll actually
	// crossed; the top row has to move at least that often.
	crossed := map[int]bool{}
	for _, yOff := range tf.scrolls {
		n := 0
		for _, u := range users {
			if u.bottom <= yOff {
				n++
			}
		}
		crossed[n] = true
	}
	if len(crossed) > 1 && len(tops) < 2 {
		t.Errorf("the recorded scroll crossed %d message boundaries but the top row never "+
			"changed: %q", len(crossed), trunc(strings.Join(tops, " | "), 120))
	}
}
