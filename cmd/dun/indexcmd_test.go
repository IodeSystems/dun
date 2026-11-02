package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/mcptable"
)

func TestIndexDetailAndStatus(t *testing.T) {
	st := dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexFailed, IndexErr: "index limit: 22012 indexable files, limit 10000",
		Exclude: []string{".claude/worktrees"}}
	if d := mcptable.ServerDetail(st); !strings.Contains(d, "running · index failed: index limit") {
		t.Errorf("detail: %q", d)
	}
	s := indexStatus(st, "lsp")
	if !strings.Contains(s, "index: failed — index limit") || !strings.Contains(s, "excluded from the index (this project): .claude/worktrees · /lsp include <dir> takes one back") {
		t.Errorf("status: %q", s)
	}
	if d := mcptable.ServerDetail(dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexBuilding, IndexSeconds: 4}); !strings.HasPrefix(d, "running · indexing 4s · ") {
		t.Errorf("detail: %q", d)
	}
}

// doctor names the binary and whether it speaks the index protocol.
func TestDoctorPolyLSP(t *testing.T) {
	dir := t.TempDir()
	write := func(name, help string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+help+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	newer := write("new", "      --index-budget string")
	older := write("old", "      --root string")
	interim := write("interim", "      --index-max-files int")
	d := &doctor{}
	d.polyLSPAt(newer, "this PATH")
	d.polyLSPAt(older, "the service's PATH")
	d.polyLSPAt(interim, "this PATH")
	if len(d.found) != 3 {
		t.Fatalf("%+v", d.found)
	}
	if f := d.found[0]; f.level != "ok" || !strings.Contains(f.detail, newer+" (on this PATH): indexes in the background; takes --exclude and --index-budget") {
		t.Errorf("new: %+v", f)
	}
	if f := d.found[2]; f.level != "warn" || !strings.Contains(f.detail, "caps files instead of a startup budget (an interim build)") {
		t.Errorf("interim: %+v", f)
	}
	if f := d.found[1]; f.level != "warn" || !strings.Contains(f.detail, older+" (on the service's PATH) is an older build: startup waits for its index") ||
		f.fix != "update it (poly-lsp: make install)" {
		t.Errorf("old: %+v", f)
	}
}

// /lsp says what the index left out, grouped by reason.
func TestIndexStatus_LeftOut(t *testing.T) {
	st := dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexReady, IndexFiles: 1304, IndexSeconds: 0.3,
		IndexLeftOut: []dun.IndexLeftOut{{Dir: ".claude/worktrees/a/", Reason: "worktree of this repo"},
			{Dir: "node_modules/", Reason: "git-ignored"}, {Dir: ".claude/worktrees/b/", Reason: "worktree of this repo"}}}
	s := indexStatus(st, "lsp")
	for _, want := range []string{"left out of the index:", "  worktree of this repo: .claude/worktrees/a/, .claude/worktrees/b/",
		"  git-ignored: node_modules/", "a path named inside one is still read"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// With leftOutBy, /lsp gives one line per reason: the true count, the
// hint, and a few of the listed examples; the list alone is the fallback.
func TestIndexStatus_LeftOutBy(t *testing.T) {
	st := dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexReady, IndexFiles: 455,
		IndexLeftOut: []dun.IndexLeftOut{{Dir: "a/", Reason: "git-ignored"}, {Dir: "b/", Reason: "git-ignored"},
			{Dir: "c/", Reason: "git-ignored"}, {Dir: "d/", Reason: "git-ignored"}, {Dir: "web/app.min.js", Reason: "minified"}},
		IndexLeftOutBy: []dun.IndexLeftOutBy{{Reason: "git-ignored", Count: 8},
			{Reason: "minified", Count: 1, Hint: "index one anyway: --include <path>"},
			{Reason: "symlink loop", Count: 2, Hint: "nothing is lost"}}}
	s := indexStatus(st, "lsp")
	for _, want := range []string{"left out of the index:", "  git-ignored: 8 dirs · e.g. a/, b/, c/, …\n",
		"  minified: 1 file — index one anyway: --include <path> · e.g. web/app.min.js\n",
		"  symlink loop: 2 links — nothing is lost\n", "a path named inside one is still read"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// A not-a-project failure names the way out; index anyway names the undo.
func TestIndexStatus_NotProjectAndAnyway(t *testing.T) {
	st := dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexFailed, IndexErr: "not a project: /home/x is the home directory"}
	if s := indexStatus(st, "lsp"); !strings.Contains(s, "/lsp index-anyway on indexes it regardless") {
		t.Errorf("no way out:\n%s", s)
	}
	st = dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexReady, IndexAnyway: true}
	if s := indexStatus(st, "lsp"); !strings.Contains(s, "/lsp index-anyway off undoes it") {
		t.Errorf("no undo:\n%s", s)
	}
	if !enginecmd.ExcludeVerb(dun.ServerCode, "index-anyway") {
		t.Error("index-anyway is not routed")
	}
}
