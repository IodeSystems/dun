package dun

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// tree writes files (rel → size in bytes, or content via body) under a new
// temp dir.
func tree(t *testing.T, files map[string]int) string {
	t.Helper()
	ws := t.TempDir()
	for rel, size := range files {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("a", size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func relFiles(t *testing.T, ws string, p IngestPlan) []string {
	t.Helper()
	var out []string
	for _, f := range p.Files {
		rel, err := filepath.Rel(ws, f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func skippedDirs(p IngestPlan) map[string]string {
	out := map[string]string{}
	for _, d := range p.Dirs {
		out[d.Rel] = d.Reason
	}
	return out
}

func TestPlanIngest_JunkNames(t *testing.T) {
	ws := tree(t, map[string]int{
		"src/main.go":                  10,
		"node_modules/left-pad/a.js":   10,
		"web/node_modules/x/index.js":  10,
		".venv/lib/x.py":               10,
		"py/env/pyvenv.cfg":            10,
		"py/env/lib/site.py":           10,
		"config/env/settings.yaml":     10, // an env/ with no pyvenv.cfg is content
		"rs/Cargo.toml":                10,
		"rs/target/debug/x.d":          10,
		"docs/target/goals.md":         10, // a target/ with no Cargo.toml beside it
		"pkg/__pycache__/m.cpython.py": 10,
		".dun/worktrees/s1/main.go":    10,
		"dist/app.js":                  10,
	})
	p := planIngest(ws, nil)
	got := relFiles(t, ws, p)
	for _, want := range []string{"src/main.go", "config/env/settings.yaml", "docs/target/goals.md", "rs/Cargo.toml"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is content and was skipped: %v", want, got)
		}
	}
	for _, f := range got {
		for _, junk := range []string{"node_modules/", ".venv/", "py/env/", "rs/target/", "__pycache__/", ".dun/", "dist/"} {
			if strings.Contains(f, junk) {
				t.Errorf("%s should be skipped (%s)", f, junk)
			}
		}
	}
	dirs := skippedDirs(p)
	for d, why := range map[string]string{"node_modules": "dependencies", "web/node_modules": "dependencies",
		".venv": "virtualenv", "py/env": "virtualenv", "rs/target": "build output", "dist": "build output"} {
		if dirs[d] != why {
			t.Errorf("skipped[%s] = %q, want %q (all: %v)", d, dirs[d], why, dirs)
		}
	}
}

func TestPlanIngest_LargeDirSkippedAtHighest(t *testing.T) {
	files := map[string]int{"src/a.go": 10, "src/b.go": 10}
	for i := range 6 {
		files["data/a/"+string(rune('a'+i))+".csv"] = 10
		files["data/b/"+string(rune('a'+i))+".csv"] = 10
	}
	for i := range 12 {
		files["big/deep/sub/"+string(rune('a'+i))+".txt"] = 10
	}
	// Bytes: three 600 KB files, each under the file limit, 1.8 MB together.
	for i := range 3 {
		files["blobs/"+string(rune('a'+i))+".txt"] = 600 << 10
	}
	ws := tree(t, files)
	p := planIngest(ws, &RagConfig{MaxDirFiles: 10, MaxDirMB: 1})
	got := relFiles(t, ws, p)
	if !reflect.DeepEqual(got, []string{"src/a.go", "src/b.go"}) {
		t.Errorf("files = %v, want src only", got)
	}
	var names []string
	for _, d := range p.Dirs {
		names = append(names, d.Rel)
	}
	slices.Sort(names)
	// data/ (12 files) and not data/a, data/b (6 each, under the limit);
	// big/ and not big/deep/sub, though only sub holds the files.
	if !reflect.DeepEqual(names, []string{"big", "blobs", "data"}) {
		t.Errorf("skipped = %v, want the highest offending dirs", names)
	}
	for _, d := range p.Dirs {
		if d.Rel == "data" && (d.Files != 12 || d.Bytes != 120) {
			t.Errorf("data/ reported %d files, %d bytes", d.Files, d.Bytes)
		}
	}
	if p.Huge {
		t.Error("what is left after the skips is small; the workspace must not be called large")
	}
}

// The workspace root is never skipped, however big: below it, per directory.
func TestPlanIngest_RootNeverSkipped(t *testing.T) {
	files := map[string]int{}
	for i := range 20 {
		files[string(rune('a'+i))+".md"] = 10
	}
	ws := tree(t, files)
	p := planIngest(ws, &RagConfig{MaxDirFiles: 5})
	if len(p.Files) != 20 || !p.Huge || len(p.Dirs) != 0 {
		t.Fatalf("files=%d huge=%v dirs=%v", len(p.Files), p.Huge, p.Dirs)
	}
	if s := p.Summary(); !strings.Contains(s, "indexing 20 files (200 B, over the directory limit — the workspace itself is large)") {
		t.Errorf("summary does not say so: %s", s)
	}
}

func TestPlanIngest_LargeAndBinaryFiles(t *testing.T) {
	ws := tree(t, map[string]int{"main.go": 10, "dump.json": 2 << 20, "logo.png": 10})
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("blob.dat", "abc\x00def")
	write("paper.pdf", "%PDF-1.7\x00binary") // raglit reads PDFs
	p := planIngest(ws, nil)
	got := relFiles(t, ws, p)
	if !reflect.DeepEqual(got, []string{"main.go", "paper.pdf"}) {
		t.Errorf("files = %v", got)
	}
	if len(p.BigFiles) != 1 || p.BigFiles[0].Rel != "dump.json" {
		t.Errorf("big = %v", p.BigFiles)
	}
	if p.Binary != 2 {
		t.Errorf("binary = %d, want 2 (logo.png by name, blob.dat by NUL)", p.Binary)
	}
}

func TestPlanIngest_IncludeAndExclude(t *testing.T) {
	files := map[string]int{
		"main.go":                    10,
		"node_modules/keep/index.js": 10,
		"node_modules/drop/index.js": 10,
		"dump.json":                  2 << 20,
		"fixtures/case.json":         10,
		"data/raw/x.csv":             10,
	}
	for i := range 12 {
		files["data/"+string(rune('a'+i))+".csv"] = 10
	}
	ws := tree(t, files)
	cfg := &RagConfig{
		Include:     []string{"node_modules/keep/", "dump.json", "data/", "*.js"},
		Exclude:     []string{"fixtures/", "data/raw/"},
		MaxDirFiles: 10,
	}
	for _, tc := range []struct {
		name string
		git  bool
	}{{"walk", false}, {"git", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := ws
			if tc.git {
				dir = gitTree(t, ws)
			}
			p := planIngest(dir, cfg)
			got := relFiles(t, dir, p)
			for _, want := range []string{"main.go", "node_modules/keep/index.js", "dump.json", "data/a.csv", "data/l.csv"} {
				if !slices.Contains(got, want) {
					t.Errorf("include must override the automatic skip of %s: %v", want, got)
				}
			}
			// *.js has no slash: it matches at any depth but does not reopen
			// a dependency directory. Exclude wins over include (data/raw).
			for _, never := range []string{"node_modules/drop/index.js", "fixtures/case.json", "data/raw/x.csv"} {
				if slices.Contains(got, never) {
					t.Errorf("%s should be skipped: %v", never, got)
				}
			}
		})
	}
}

// gitTree copies ws into a fresh git repo, with node_modules untracked but not
// ignored — the case git's own list lets through.
func gitTree(t *testing.T, ws string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("cp", "-a", ws+"/.", dir).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestPlanIngest_GitAndWalk(t *testing.T) {
	ws := tree(t, map[string]int{
		"main.go":               10,
		"node_modules/x/a.js":   10,
		"secret/key.txt":        10,
		"app.log":               10,
		"sub/gen/out.go":        10,
		"sub/keep.go":           10,
		"huge.sql":              3 << 20,
		"vendor/lib/lib.go":     10, // vendor is judged by size, not name
		".claude/worktrees/x/y": 10,
	})
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("secret/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", ".gitignore"), []byte("gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "main.go", "sub/.gitignore", "sub/keep.go", "vendor/lib/lib.go"}
	for _, tc := range []struct {
		name string
		git  bool
	}{{"walk", false}, {"git", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := ws
			if tc.git {
				dir = gitTree(t, ws)
			}
			p := planIngest(dir, nil)
			if got := relFiles(t, dir, p); !reflect.DeepEqual(got, want) {
				t.Errorf("files = %v, want %v", got, want)
			}
			if skippedDirs(p)["node_modules"] != "dependencies" {
				t.Errorf("node_modules not reported: %v", p.Dirs)
			}
			if len(p.BigFiles) != 1 || p.BigFiles[0].Rel != "huge.sql" {
				t.Errorf("big = %v", p.BigFiles)
			}
		})
	}
}

func TestIngestPlan_Summary(t *testing.T) {
	p := IngestPlan{
		Files: make([]string, 1234),
		Dirs: []SkippedDir{
			{Rel: "node_modules", Reason: "dependencies"},
			{Rel: ".git", Reason: "git"},
			{Rel: "data", Files: 12345, Bytes: 2469606195},
		},
		BigFiles: []SkippedFile{{"dumps/dump.json", 310 << 20}, {"b.csv", 2 << 20}},
		Binary:   3,
		MaxFile:  1 << 20,
	}
	want := `docs: indexing 1,234 files · skipped node_modules/ (dependencies), data/ (12,345 files, 2.3 GB), ` +
		`2 files over 1 MB (largest: dump.json 310 MB, b.csv 2 MB), 3 binary files — ` +
		`to include one: "rag": {"include": ["data/"]} in dun.json`
	if got := p.Summary(); got != want {
		t.Errorf("summary:\n got %s\nwant %s", got, want)
	}

	if got := (IngestPlan{Files: make([]string, 1)}).Summary(); got != "docs: indexing 1 file" {
		t.Errorf("nothing skipped: %s", got)
	}

	var many []SkippedDir
	for i := range 7 {
		many = append(many, SkippedDir{Rel: "d" + string(rune('0'+i)), Reason: "cache"})
	}
	if got := (IngestPlan{Files: make([]string, 2), Dirs: many}).Summary(); !strings.Contains(got, "d4/ (cache), +2 more —") {
		t.Errorf("at most five dirs, then +N more: %s", got)
	}
}

func TestLoadRag_Layers(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if LoadRag(dir) != nil {
		t.Fatal("no section anywhere must be nil (the defaults)")
	}
	write(ProjectServersFile, `{"rag":{"include":["data/"],"exclude":["fixtures/"],"max_file_kb":2048,"max_dir_files":100}}`)
	write(LocalServersFile, `{"rag":{"include":["docs/big.json"],"max_dir_files":9000}}`)
	c := LoadRag(dir)
	if c == nil {
		t.Fatal("nil")
	}
	if !reflect.DeepEqual(c.Include, []string{"data/", "docs/big.json"}) || !reflect.DeepEqual(c.Exclude, []string{"fixtures/"}) {
		t.Errorf("patterns must accumulate: %+v", c)
	}
	if c.MaxFileKB != 2048 || c.MaxDirFiles != 9000 {
		t.Errorf("limits: %+v (an unstated one inherits, a stated one wins)", c)
	}
	f, files, b := c.limits()
	if f != 2048<<10 || files != 9000 || b != defaultRagMaxDirMB<<20 {
		t.Errorf("limits() = %d %d %d", f, files, b)
	}
}
