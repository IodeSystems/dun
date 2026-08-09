package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Replay the trace file's layout dump against the scrollOverlay logic
// directly — no need to rebuild the conversation, just use the real
// rowOffset/blockH values from the trace.
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

	type userMsg struct {
		userText  string
		rowOffset int
		blockH    int
	}
	var msgs []userMsg
	var scrolls []int

	layoutRe := regexp.MustCompile(`^layout\s+(\d+)\s+kind=(\w+)\s+userText="([^"]*)"\s+rowOffset=(\d+)\s+h=(\d+)$`)
	scrollRe := regexp.MustCompile(`^scroll\s+yoff=(\d+)\s+pinned=(\w+)$`)

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if m := layoutRe.FindStringSubmatch(line); m != nil && m[2] == "user" {
			rowOffset, _ := strconv.Atoi(m[4])
			bh, _ := strconv.Atoi(m[5])
			msgs = append(msgs, userMsg{userText: m[3], rowOffset: rowOffset, blockH: bh})
			continue
		}

		if m := scrollRe.FindStringSubmatch(line); m != nil {
			yoff, _ := strconv.Atoi(m[1])
			scrolls = append(scrolls, yoff)
			continue
		}
	}

	if len(msgs) < 2 {
		t.Skipf("need at least 2 user messages, got %d", len(msgs))
	}

	for _, u := range msgs {
		t.Logf("user=%q rowOffset=%d h=%d bottom=%d",
			trunc(u.userText, 60), u.rowOffset, u.blockH, u.rowOffset+u.blockH)
	}

	// Simulate the scrollOverlay logic with real rowOffset/blockH values
	scrollRange := make(map[int]bool)
	for _, y := range scrolls {
		scrollRange[y] = true
	}
	if len(scrollRange) == 0 {
		// No scroll events — simulate full range
		maxBottom := 0
		for _, u := range msgs {
			b := u.rowOffset + u.blockH
			if b > maxBottom {
				maxBottom = b
			}
		}
		for y := 0; y <= maxBottom; y += 3 {
			scrollRange[y] = true
		}
	}

	// For each YOffset, compute what the overlay would show
	var seen []string
	for yOff := 0; yOff <= 4000; yOff++ {
		if !scrollRange[yOff] {
			continue
		}

		// scrollOverlay logic: find off-screen message closest to viewport top
		var closestText string
		closestBottom := -1
		for _, u := range msgs {
			bottom := u.rowOffset + u.blockH
			if bottom <= yOff && bottom > closestBottom {
				closestBottom = bottom
				closestText = u.userText
			}
		}

		if closestText != "" {
			truncated := trunc(closestText, 40)
			entry := fmt.Sprintf("yoff=%d %s", yOff, truncated)
			if len(seen) == 0 || seen[len(seen)-1] != entry {
				seen = append(seen, entry)
			}
		}
	}

	// Only print transitions
	t.Logf("overlay transitions (%d):", len(seen))
	for _, s := range seen {
		t.Logf("  %s", s)
	}

	// Check all messages appeared
	for _, u := range msgs {
		found := false
		for _, s := range seen {
			if strings.Contains(s, trunc(u.userText, 40)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("never saw %q in overlay", trunc(u.userText, 60))
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
