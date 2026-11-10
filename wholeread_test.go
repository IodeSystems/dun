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
	if strings.Contains(out, "over a quarter") {
		t.Error("under a quarter got the stronger cue")
	}
}

// Over a quarter of the room, under all of it (USER 2026-11-09): served,
// with the stronger cue.
func TestWholeRead_OverAQuarter(t *testing.T) {
	h, ws := wholeHarness(t, 200000, 0)
	writeLines(t, filepath.Join(ws, "big.txt"), 3000, 60) // ~213KB, ~53k tokens of ~165k free
	out := callTool(t, h, "file_read", map[string]any{"path": "big.txt", "whole": true})
	if !strings.HasPrefix(out, "big.txt: 3000 lines\n") || !strings.Contains(out, "\n3000\tline 3000 ") {
		t.Fatalf("not served whole: %.200q", out)
	}
	if strings.Count(out, "This read is") != 1 ||
		!strings.Contains(out, "tokens, over a quarter of the room left. Take what you need from it and recap it right after, this turn or the next.") {
		t.Errorf("cue: %q", out[strings.LastIndex(out, "3000\t"):])
	}
}

// Past the room: refused with the numbers; the root has the
// agent tool, so delegation is offered.
func TestWholeRead_BlowsContext(t *testing.T) {
	h, ws := wholeHarness(t, 200000, 100000)
	writeLines(t, filepath.Join(ws, "big.txt"), 4000, 65) // ~304KB, ~76k tokens of ~65k free
	out := callTool(t, h, "file_read", map[string]any{"path": "big.txt", "whole": true})
	for _, want := range []string{"ERROR: big.txt is ", " are free. Ways out:", "1. Recap what is settled to free at least ",
		`2. Add a plan node "read big.txt and report …", then agent({node: "<id>"}): a child spends its own context`, "3. Grep it first, or read a range."} {
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
	out := wholeVerdict("x.log", 80000, wholeRoom{known: true, free: 65000, inUse: 100000, empty: 165000})
	if !strings.Contains(out, "ERROR: x.log is 80000 tokens; 65000 are free. Ways out:") ||
		!strings.Contains(out, "1. Recap what is settled to free at least 15000 tokens") ||
		!strings.Contains(out, "2. Grep it first") || strings.Contains(out, "agent(") {
		t.Errorf("no agent tool:\n%s", out)
	}
	// With the agent tool.
	out = wholeVerdict("x.log", 80000, wholeRoom{known: true, free: 65000, inUse: 100000, empty: 165000, agent: true})
	if !strings.Contains(out, `2. Add a plan node "read x.log and report …", then agent({node: "<id>"})`) || !strings.Contains(out, "3. Grep") {
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
	if out := wholeVerdict("x.log", 65000, wholeRoom{known: true, free: 65000}); out != "" {
		t.Errorf("fits: %s", out)
	}
	if wholeAsideFor(16000, wholeRoom{known: true, free: 65000}) != wholeAside ||
		wholeAsideFor(17000, wholeRoom{known: true, free: 65000}) != wholeAsideBig {
		t.Error("the cue's wording does not turn at a quarter of the room")
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
