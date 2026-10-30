package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// A running poly-lsp whose index builds says indexing on the splash and the
// status line; when the build ends the words go, and an index report is
// said in the conversation even beside the live splash.
func TestIndexing_SplashStatusAndReport(t *testing.T) {
	v := newVtui(100, 21)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	servers := func(index string) []any {
		return []any{
			map[string]any{"id": "shell", "running": true, "tools": float64(1)},
			map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "auto": true, "running": true,
				"index": index, "indexSeconds": float64(12), "tools": float64(4)},
		}
	}
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": servers(dun.IndexBuilding)})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	if got := stripANSI(convoText(v.m)); !strings.Contains(got, "poly-lsp indexing 12s") {
		t.Fatalf("the splash does not say indexing:\n%s", got)
	}
	if got := lastRow(v.m); !strings.Contains(got, "ready · poly-lsp indexing 12s") {
		t.Fatalf("status: %q", got)
	}
	n := len(v.m.convo)
	msg := "poly-lsp: the index holds .claude/worktrees/ (21,557 files, 97% of the index, 48 other git checkouts). To leave it out: /lsp exclude .claude/worktrees"
	v.event(map[string]any{"type": "server", "id": "poly-lsp", "action": "index", "background": true,
		"message": msg, "servers": servers(dun.IndexReady), "tools": []any{"eval", "node_query"}})
	if len(v.m.convo) <= n || !strings.Contains(stripANSI(convoText(v.m)), "the index holds .claude/worktrees/") {
		t.Fatalf("the report is not in the conversation:\n%s", convoText(v.m))
	}
	if got := lastRow(v.m); strings.Contains(got, "indexing") {
		t.Fatalf("still indexing after ready: %q", got)
	}
	// The splash's tools row names a server only while it is changing or wrong.
	if got := stripANSI(convoText(v.m)); strings.Contains(got, "poly-lsp indexing") {
		t.Fatalf("the splash still says indexing:\n%s", got)
	}
}

func TestIndexDetailAndStatus(t *testing.T) {
	st := dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexFailed, IndexErr: "index limit: 22012 indexable files, limit 10000",
		Exclude: []string{".claude/worktrees"}}
	if d := serverDetail(st); !strings.Contains(d, "running · index failed: index limit") {
		t.Errorf("detail: %q", d)
	}
	s := indexStatus(st, "lsp")
	if !strings.Contains(s, "index: failed — index limit") || !strings.Contains(s, "excluded from the index (this project): .claude/worktrees · /lsp include <dir> takes one back") {
		t.Errorf("status: %q", s)
	}
	if d := serverDetail(dun.ServerState{ID: "poly-lsp", Running: true, Index: dun.IndexBuilding, IndexSeconds: 4}); !strings.HasPrefix(d, "running · indexing 4s · ") {
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
	if !excludeVerb(dun.ServerCode, "index-anyway") {
		t.Error("index-anyway is not routed")
	}
}
