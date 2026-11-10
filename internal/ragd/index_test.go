package ragd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

const demoGo = `package demo

// Alpha counts the walrus herd.
func Alpha() int {
	return 1
}

func Beta() string {
	return "zeppelin"
}
`

const demoMd = `# Guide

Intro text.

## Install

Run the harpoon installer.

## Usage

Feed the narwhal daily.
`

// indexRepo is a git repo with a Go file, a Markdown file and a gitignored
// file, for the index tests.
func indexRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{
		"demo.go":    demoGo,
		"guide.md":   demoMd,
		".gitignore": "secret.go\n",
		"secret.go":  "package demo\n\n// Hidden mentions the walrus too.\nfunc Hidden() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// runIndex is the rag_index handler's work: the workspace rules, then Index.
func runIndex(t *testing.T, h *Host, root string) *proto.RagIndexed {
	t.Helper()
	files, _ := dun.IndexFiles(root)
	r, err := h.Index(proto.RagIndex{Project: "dun", Root: root}, files)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, "dun", "code")
	waitIdle(t, h, "dun", "docs")
	return r
}

func oneHit(t *testing.T, h *Host, index, query string) proto.RagHit {
	t.Helper()
	r := search(t, h, proto.RagSearch{Project: "dun", Index: index, Query: query, Mode: "fts"})
	if len(r.Hits) == 0 {
		t.Fatalf("%s: no hit for %q", index, query)
	}
	return r.Hits[0]
}

// The code and docs indexes from a directory: code fragments carry a symbol
// and a line span, docs fragments the heading path; a re-run queues
// nothing; a deleted file is forgotten; a gitignored file is skipped.
func TestIndexCodeAndDocs(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	root := indexRepo(t)

	r := runIndex(t, h, root)
	// code: demo.go and .gitignore (no extension: not prose); secret.go is
	// ignored.
	if r.Code.Queued != 2 || r.Docs.Queued != 1 || r.Current != 0 || r.Removed != 0 {
		t.Fatalf("first index: %+v, want 2 code and 1 docs file queued", r)
	}

	a := oneHit(t, h, "code", "walrus")
	if a.Path != "file://"+filepath.Join(root, "demo.go") {
		t.Fatalf("walrus hit is %s: the gitignored secret.go was indexed?", a.Path)
	}
	if a.Symbol != "Alpha" || a.StartLine != 3 || a.EndLine != 8 {
		t.Errorf("Alpha hit: symbol %q lines [%d,%d), want Alpha [3,8) (the doc comment in; fragments tile the file, so the blank line after too)", a.Symbol, a.StartLine, a.EndLine)
	}
	if b := oneHit(t, h, "code", "zeppelin"); b.Symbol != "Beta" || b.StartLine != 8 || b.EndLine != 11 {
		t.Errorf("Beta hit: symbol %q lines [%d,%d), want Beta [8,11)", b.Symbol, b.StartLine, b.EndLine)
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "docs", Query: "walrus", Mode: "fts"}); len(r.Hits) != 0 {
		t.Errorf("demo.go went to docs too: %+v", r.Hits)
	}
	inst := oneHit(t, h, "docs", "harpoon")
	if !strings.Contains(inst.Symbol, "Install") || !strings.Contains(inst.Symbol, "Guide") || inst.StartLine != 5 {
		t.Errorf("Install hit: symbol %q line %d, want the heading path Guide…Install at line 5", inst.Symbol, inst.StartLine)
	}
	if u := oneHit(t, h, "docs", "narwhal"); !strings.Contains(u.Symbol, "Usage") {
		t.Errorf("Usage hit: symbol %q, want the heading path to Usage", u.Symbol)
	}

	if r := runIndex(t, h, root); r.Queued != 0 || r.Current != 3 || r.Removed != 0 {
		t.Fatalf("re-run of an unchanged tree: %+v, want nothing queued, 3 current", r)
	}

	if err := os.Remove(filepath.Join(root, "guide.md")); err != nil {
		t.Fatal(err)
	}
	if r := runIndex(t, h, root); r.Docs.Removed != 1 || r.Queued != 0 || r.Code.Current != 2 {
		t.Fatalf("after deleting guide.md: %+v, want docs removed 1", r)
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "docs", Query: "harpoon", Mode: "fts"}); len(r.Hits) != 0 {
		t.Errorf("a deleted file is still found: %+v", r.Hits)
	}
}

// A file indexed with no segmenter is queued Fresh on the first index with
// one, so its declaration fragments are made though its bytes did not
// change; the next run queues nothing.
func TestIndexFreshOnNewSegmenter(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ragRoot := t.TempDir()
	root := indexRepo(t)

	// As before the segmenter: text windows, no segmenter record.
	newCodeSeg = func() raglit.CodeSegmenter { return nil }
	h, err := Open(ragRoot, Roles{}, t.Logf)
	newCodeSeg = func() raglit.CodeSegmenter { return codeSeg{} }
	if err != nil {
		t.Fatal(err)
	}
	runIndex(t, h, root)
	if a := oneHit(t, h, "code", "walrus"); a.Symbol != "" {
		t.Fatalf("with no segmenter: symbol %q", a.Symbol)
	}
	h.Close(time.Second)

	h = open(t, ragRoot)
	if r := runIndex(t, h, root); r.Queued != 3 {
		t.Fatalf("first run with the segmenter: %+v, want all 3 files queued fresh", r)
	}
	if a := oneHit(t, h, "code", "walrus"); a.Symbol != "Alpha" {
		t.Errorf("after the fresh run: symbol %q, want Alpha", a.Symbol)
	}
	if r := runIndex(t, h, root); r.Queued != 0 {
		t.Fatalf("second run with the segmenter: %+v, want nothing queued", r)
	}
}

// Runtime names the walk's module version (go.mod's, whether or not the
// test binary's build info lists modules) and the pure-Go parser.
func TestCodeSegRuntime(t *testing.T) {
	rt := codeSeg{}.Runtime()
	if !strings.HasPrefix(rt, "poly-lsp-mcp/segment@v0.5.1 gotreesitter") {
		t.Fatalf("Runtime() = %q, want poly-lsp-mcp/segment@v0.5.1 gotreesitter…", rt)
	}
}

// A file raglit refuses (minified) is not queued again until it changes.
func TestIndexFailedFileWaitsForAChange(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	root := indexRepo(t)
	min := filepath.Join(root, "bundle.js")
	if err := os.WriteFile(min, []byte("var a=1;"+strings.Repeat("a+=1;", 2000)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runIndex(t, h, root); r.Code.Queued != 3 {
		t.Fatalf("first index: %+v, want 3 code files queued", r)
	}
	if r := runIndex(t, h, root); r.Queued != 0 || r.Failed != 1 {
		t.Fatalf("re-run: %+v, want nothing queued and bundle.js failed", r)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(min, later, later); err != nil {
		t.Fatal(err)
	}
	if r := runIndex(t, h, root); r.Queued != 1 || r.Failed != 0 {
		t.Fatalf("after bundle.js changed: %+v, want it queued again", r)
	}
}
