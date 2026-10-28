package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filetools "github.com/iodesystems/agentkit/files"
)

// The step one-liners, built from REAL toolset output: the renderers parse
// the first line agentkit/files writes, so a wording change there must fail
// here rather than silently degrade the preview to a clipped sentence.
func TestFileToolStepLines(t *testing.T) {
	root := t.TempDir()
	write := func(p, body string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var src strings.Builder
	for i := 0; i < 60; i++ {
		src.WriteString("line\n")
	}
	write("internal/a.go", "package a\n\nfunc A() int {\n\treturn 1\n}\n")
	write("big.txt", src.String())
	write("internal/b.go", "package a\n// needle\n")
	write("one.txt", "needle\n")
	ts, err := filetools.New(filetools.Config{Root: root, Tools: []string{"read", "edit", "write", "grep", "list", "delete", "move"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tool   string
		args   map[string]any
		want   string
		failed bool
	}{
		{"file_read", map[string]any{"path": "internal/a.go"}, "read internal/a.go (5 lines)", false},
		{"file_read", map[string]any{"path": "big.txt", "offset": 10, "limit": 31}, "read big.txt:10-40 (31 lines of 60)", false},
		{"file_read", map[string]any{"path": "nope.go"}, "read nope.go → nope.go: does not exist.", true},
		{"file_edit", map[string]any{"path": "internal/a.go", "old": "return 1", "new": "x := 2\n\treturn x"}, "edit internal/a.go +2 −1", false},
		{"file_edit", map[string]any{"path": "internal/a.go", "old": "nothing like it", "new": "x"}, "edit internal/a.go → \"nothing like it\" does not appear", true},
		{"file_write", map[string]any{"path": "docs/new.md", "content": "# t\n\nbody\n"}, "write docs/new.md (3 lines, new)", false},
		{"file_write", map[string]any{"path": "docs/new.md", "content": "x\n"}, "write docs/new.md (1 line, overwritten)", false},
		{"file_grep", map[string]any{"pattern": "needle"}, "grep /needle/ → 2 matches in 2 files", false},
		{"file_grep", map[string]any{"pattern": "needle", "path": "internal", "glob": "*.go"}, "grep /needle/ in internal *.go → 1 match in 1 file", false},
		{"file_grep", map[string]any{"pattern": "needle(", "files": true}, "grep /needle(/ → no matches", false},
		{"file_grep", map[string]any{"pattern": "needle", "files": true}, "grep /needle/ → 2 files", false},
		{"file_list", map[string]any{}, "list → 2 dirs, 2 files", false},
		{"file_list", map[string]any{"glob": "**/*.go"}, "list **/*.go → 2 files", false},
		// file_read searches with grep and lists a directory (agentkit v0.14).
		{"file_read", map[string]any{"path": ".", "grep": "needle"}, "grep /needle/ → 2 matches in 2 files", false},
		{"file_read", map[string]any{"path": "internal", "grep": "needle", "glob": "*.go"}, "grep /needle/ in internal *.go → 1 match in 1 file", false},
		{"file_read", map[string]any{"path": "internal/b.go", "grep": "needle", "context": 1}, "grep /needle/ in internal/b.go → 1 match in 1 file", false},
		{"file_read", map[string]any{"path": "one.txt", "grep": "absent"}, "grep /absent/ in one.txt → no matches", false},
		{"file_read", map[string]any{"path": "internal"}, "read internal → 0 dirs, 2 files", false},
		{"file_read", map[string]any{"path": ".", "glob": "**/*.go"}, "read . → 2 files", false},
		{"file_move", map[string]any{"path": "one.txt", "to": "two.txt"}, "move one.txt → two.txt ✓", false},
		{"file_delete", map[string]any{"path": "two.txt"}, "delete two.txt ✓", false},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.args)
		result := ts.Dispatch(context.Background(), c.tool, string(b))
		preview, body := renderToolResult(renderCtx{tool: c.tool, args: c.args, result: result, width: 100})
		line := stripANSI(callText(c.tool, c.args, 80) + preview)
		if !strings.HasPrefix(line, c.want) {
			t.Errorf("%s %v\n got %q\nwant %q\nresult: %s", c.tool, c.args, line, c.want, result)
		}
		if got := toolFailed(c.tool, result); got != c.failed {
			t.Errorf("%s: toolFailed = %v, want %v", c.tool, got, c.failed)
		}
		if strings.TrimSpace(stripANSI(body)) == "" {
			t.Errorf("%s: empty body", c.tool)
		}
		if c.tool == "file_edit" && !c.failed && !strings.Contains(stripANSI(body), "x := 2") {
			t.Errorf("edit body lacks the hunk:\n%s", stripANSI(body))
		}
	}
}

// Consecutive whole-file reads group under the verb, not the tool's name.
func TestFileToolGroupLine(t *testing.T) {
	items := []*runItem{
		{tool: "file_read", args: map[string]any{"path": "a.go"}, done: true},
		{tool: "file_read", args: map[string]any{"path": "b.go"}, done: true},
	}
	if n := groupLen(items); n != 2 {
		t.Fatalf("groupLen = %d", n)
	}
	if got := stripANSI(groupLine(items, 80)); got != "✓ read ×2: a.go, b.go" {
		t.Fatalf("group line = %q", got)
	}
}
