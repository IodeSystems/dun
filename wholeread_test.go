package dun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// lines writes n numbered lines of about width bytes each to path.
func writeLines(t *testing.T, path string, n, width int) {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", width))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wholeHarness(t *testing.T, window, prompt int) (*Harness, string) {
	t.Helper()
	h, ws := itemHarness(t, &fakeRag{})
	h.window.Store(int64(window))
	if prompt > 0 {
		h.meter.NoteBuild(prompt*4, prompt)
	}
	return h, ws
}

// Under wholeSmallTokens: all of the file, past the 8KB unranged cap, and
// no cue.
func TestWholeRead_Small(t *testing.T) {
	h, ws := wholeHarness(t, 200000, 0)
	writeLines(t, filepath.Join(ws, "a.txt"), 400, 30) // ~16KB, ~4k tokens
	out := callTool(t, h, "file_read", map[string]any{"path": "a.txt", "whole": true})
	if !strings.HasPrefix(out, "a.txt: 400 lines\n") || !strings.Contains(out, "line 400 ") {
		t.Fatalf("small whole read: %.200q", out)
	}
	if strings.Contains(out, "This read is") {
		t.Error("a small read got the cue")
	}
	// Without whole the cap holds.
	if out := callTool(t, h, "file_read", map[string]any{"path": "a.txt"}); strings.Contains(out, "line 400 ") {
		t.Error("an unranged read is still capped")
	}
}

// Large, under a quarter of the room: every page, one header, unclipped,
// and the recap cue once.
func TestWholeRead_LargeFits(t *testing.T) {
	h, ws := wholeHarness(t, 400000, 0)
	writeLines(t, filepath.Join(ws, "big.txt"), 3000, 50) // ~180KB: three pages
	out := callTool(t, h, "file_read", map[string]any{"path": "big.txt", "whole": true})
	if !strings.HasPrefix(out, "big.txt: 3000 lines\n") {
		t.Fatalf("header: %.120q", out)
	}
	for i := 1; i <= 3000; i++ {
		if !strings.Contains(out, fmt.Sprintf("\n%d\tline %d %s\n", i, i, strings.Repeat("x", 50))) {
			t.Fatalf("line %d missing or clipped", i)
		}
	}
	if strings.Contains(out, "[cut at") || strings.Contains(out, "lines 1-") {
		t.Error("a page's header or cut note is in the whole read")
	}
	// The cue rides the result (the outer lift, notify.go), once.
	if strings.Count(out, "This read is") != 1 || !strings.Contains(out, "Take what you need from it, then recap it on your next turn.") {
		t.Errorf("cue: %q", out[strings.LastIndex(out, "3000\t"):])
	}
}

// Past a quarter of the room: refused with the numbers; the root has the
// agent tool, so delegation is offered.
func TestWholeRead_BlowsContext(t *testing.T) {
	h, ws := wholeHarness(t, 200000, 100000)
	writeLines(t, filepath.Join(ws, "big.txt"), 2000, 50) // ~120KB, ~30k tokens
	out := callTool(t, h, "file_read", map[string]any{"path": "big.txt", "whole": true})
	for _, want := range []string{"ERROR: big.txt is ", " tokens; a whole read may take ", "1. Recap what is settled to free at least ",
		`2. agent({prompt: "read big.txt and report …"})`, "3. Grep it first, or read a range."} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line 1 ") {
		t.Error("a refused read returned the file")
	}
}

func TestWholeRead_RefusalVariants(t *testing.T) {
	// Recap can free enough; no agent tool (a child): recap, then narrow.
	out := wholeVerdict("x.log", 30000, wholeRoom{known: true, free: 65000, inUse: 100000, empty: 165000})
	if !strings.Contains(out, "x.log is 30000 tokens; a whole read may take 16250, a quarter of the 65000 free") ||
		!strings.Contains(out, "1. Recap what is settled to free at least 55000 tokens") ||
		!strings.Contains(out, "2. Grep it first") || strings.Contains(out, "agent(") {
		t.Errorf("no agent tool:\n%s", out)
	}
	// With the agent tool.
	out = wholeVerdict("x.log", 30000, wholeRoom{known: true, free: 65000, inUse: 100000, empty: 165000, agent: true})
	if !strings.Contains(out, `2. agent({prompt: "read x.log and report …"})`) || !strings.Contains(out, "3. Grep") {
		t.Errorf("agent tool:\n%s", out)
	}
	// Larger than a child's window: no recap (it cannot free enough), no
	// delegation, narrow.
	out = wholeVerdict("x.log", 200000, wholeRoom{known: true, free: 65000, inUse: 100000, empty: 165000, agent: true})
	if strings.Contains(out, "Recap") || strings.Contains(out, "agent(") ||
		!strings.Contains(out, "1. It is larger than a child's whole window: narrow it.") || !strings.Contains(out, "2. Grep") {
		t.Errorf("larger than a child:\n%s", out)
	}
	// Unknown window: large is refused, small is not.
	if out := wholeVerdict("x.log", 6000, wholeRoom{}); !strings.Contains(out, "the context window is unknown") {
		t.Errorf("unknown window: %s", out)
	}
	if out := wholeVerdict("x.log", 4000, wholeRoom{}); out != "" {
		t.Errorf("small with no window: %s", out)
	}
	if out := wholeVerdict("x.log", 16000, wholeRoom{known: true, free: 65000}); out != "" {
		t.Errorf("fits: %s", out)
	}
}

// whole on an item path: the item, as without it.
func TestWholeRead_ItemPath(t *testing.T) {
	h, _ := wholeHarness(t, 200000, 0)
	if _, err := h.MemoriesCommand("add strategy bisect-flaky Run the flaky test alone with -count=20."); err != nil {
		t.Fatal(err)
	}
	callTool(t, h, "memory", map[string]any{"action": "add", "name": "cgo", "question": "how to build?", "content": "Builds need CGO off."})
	if out := callTool(t, h, "file_read", map[string]any{"path": "strategy:bisect-flaky", "whole": true}); !strings.Contains(out, "-count=20") {
		t.Errorf("strategy: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "memory:cgo", "whole": true}); !strings.Contains(out, "CGO off") {
		t.Errorf("memory: %q", out)
	}
}

// whole with a range is refused; whole with grep is a grep.
func TestWholeRead_ArgCombos(t *testing.T) {
	h, ws := wholeHarness(t, 200000, 0)
	writeLines(t, filepath.Join(ws, "a.txt"), 10, 5)
	if out := callTool(t, h, "file_read", map[string]any{"path": "a.txt", "whole": true, "offset": 3}); !strings.HasPrefix(out, "ERROR: whole reads all") {
		t.Errorf("whole+offset: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "a.txt", "whole": true, "grep": "line 7 "}); !strings.Contains(out, ":7:") {
		t.Errorf("whole+grep: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "nope.txt", "whole": true}); !strings.HasPrefix(out, "ERROR") {
		t.Errorf("missing file: %q", out)
	}
}

// The schema gains whole and one description line.
func TestWholeRead_Schema(t *testing.T) {
	h, _ := wholeHarness(t, 200000, 0)
	var def *llm.ToolDef
	for i, td := range h.Session.Tools {
		if td.Function.Name == "file_read" {
			def = &h.Session.Tools[i]
		}
	}
	if def == nil {
		t.Fatal("no file_read")
	}
	props := def.Function.Parameters.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["whole"]; !ok || !strings.HasSuffix(def.Function.Description, wholeDesc) {
		t.Errorf("whole not in the schema: %v / %q", props["whole"], def.Function.Description)
	}
}
