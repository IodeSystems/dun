package dun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Background jobs have always streamed to a file and handed the model a bounded
// tail. The foreground path never did: one `cat` of a 252 KB log put 255,720
// characters — about 64k tokens — into the window to answer a question whose
// answer was "30", and every later turn paid for it again.
func TestCapExecOutput_BoundsWhatReachesTheModel(t *testing.T) {
	var saved string
	dir := t.TempDir()
	spill := func(command, output string) string {
		p := filepath.Join(dir, "out.txt")
		os.WriteFile(p, []byte(output), 0o600)
		saved = output
		return p
	}

	// Small output is untouched — the cap must be invisible in the normal case.
	small := "line one\nline two\n"
	if got := capExecOutput(small, "echo hi", spill); got != small {
		t.Fatalf("a small result must pass through unchanged, got %q", got)
	}

	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("log line ")
		b.WriteString(strings.Repeat("x", 10))
		b.WriteString("\n")
	}
	full := "FIRST LINE\n" + b.String() + "LAST LINE\n[exit: status 2]"
	got := capExecOutput(full, "cat big.log", spill)

	if len(got) > execInlineMax+400 {
		t.Errorf("the clipped result is still %d characters", len(got))
	}
	// BOTH ends survive: a failure is at the bottom, a listing is useful from
	// the top, and the foreground path sees both.
	if !strings.Contains(got, "FIRST LINE") {
		t.Error("the start of the output must survive")
	}
	if !strings.Contains(got, "LAST LINE") {
		t.Error("the end of the output must survive")
	}
	// The verdict lives at the very end, so clipping must never eat it.
	if !strings.Contains(got, "[exit: status 2]") {
		t.Error("the exit marker must survive the clip")
	}
	if !strings.Contains(got, "characters elided") || !strings.Contains(got, "out.txt") {
		t.Errorf("the gap must say how much went and where it is: %q", got[:200])
	}
	// Nothing is lost: the file has all of it.
	if saved != full {
		t.Error("the spilled file must hold the complete output")
	}
	// Cut on line boundaries — half a line reads as corruption, not truncation.
	for _, part := range strings.Split(got, "…") {
		_ = part
	}
	if strings.Contains(got, "xxxxxxxxxx…[") {
		t.Error("clipped mid-line")
	}
}

// With nowhere to save it, the result is still clipped — the context cost is
// the problem being solved, and saying so is better than silently spending it.
func TestCapExecOutput_ClipsEvenWithNoSpill(t *testing.T) {
	full := strings.Repeat("noise line\n", 5000)
	got := capExecOutput(full, "cat x", nil)
	if len(got) > execInlineMax+400 {
		t.Errorf("still %d characters with no spill", len(got))
	}
	if !strings.Contains(got, "not saved") {
		t.Errorf("it must admit the rest is gone: %q", got[:200])
	}
}
