package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWalkSizes(t *testing.T) {
	cases := []struct {
		name string
		w    walk
		over []string
		want []size
	}{
		{"list", walk{Sizes: []string{"48x24", "100x30", "180×50"}}, nil, []size{{48, 24}, {100, 30}, {180, 50}}},
		{"old walk", walk{Cols: 80, Rows: 25}, nil, []size{{80, 25}}},
		{"neither", walk{}, nil, []size{{100, 30}}},
		{"override wins", walk{Sizes: []string{"48x24"}, Cols: 80, Rows: 25}, []string{"120x40"}, []size{{120, 40}}},
		{"list beats cols", walk{Sizes: []string{"48x24"}, Cols: 80, Rows: 25}, nil, []size{{48, 24}}},
	}
	for _, c := range cases {
		got, err := walkSizes(c.w, c.over)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v %v, want %v", c.name, got, err, c.want)
		}
	}
	for _, bad := range []string{"48", "48x", "x24", "4x24", "48x24x2", "wide"} {
		if _, err := walkSizes(walk{Sizes: []string{bad}}, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := walkSizes(walk{Sizes: []string{"48x24", "48x24"}}, nil); err == nil {
		t.Error("a size twice accepted")
	}
}

// One run directory is one size: its brief says that size and names exactly
// the files in it that the persona may read.
func TestAssemble_BriefIsTheRunsSize(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	walkPath := filepath.Join(dir, "w.json")
	write(walkPath, `{"name":"w","sizes":["48x24","180x50"],"steps":[{"id":"first","promise":"p"}]}`)
	write(filepath.Join(dir, "s.md"), "scenario")
	write(filepath.Join(dir, "rec.json"), `{"ok":true,"steps":[{"id":"first","ok":true,"did":["looked"]}]}`)
	write(filepath.Join(run, "render.json"), `{"cell_w":10,"cell_h":20,"marks":[{"n":1,"label":"first","png":"01-first.png","txt":"01-first.txt"}]}`)
	write(filepath.Join(run, "01-first.png"), "png")
	write(filepath.Join(run, "01-first.txt"), "screen")

	args := []string{"-walk", walkPath, "-scenario", filepath.Join(dir, "s.md"), "-run", run, "-result", filepath.Join(dir, "rec.json")}
	if err := runAssemble(args); err == nil || !strings.Contains(err.Error(), "-size") {
		t.Fatalf("two sizes and no -size: err %v", err)
	}
	write(filepath.Join(run, "01-first.txt"), "screen")
	if err := runAssemble(append(args, "-size", "48x24")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run, "BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	brief := string(b)
	if !strings.Contains(brief, "48 columns by 24 rows") {
		t.Errorf("brief does not say this run's size:\n%s", brief)
	}
	_, list, _ := strings.Cut(brief, "## Every file you may read")
	list, _, _ = strings.Cut(list, "Nothing else")
	var named []string
	for _, ln := range strings.Split(list, "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(ln), "- `"); ok {
			named = append(named, strings.TrimSuffix(f, "`"))
		}
	}
	if want := []string{"BRIEF.md", "01-first.png", "01-first.txt"}; !reflect.DeepEqual(named, want) {
		t.Errorf("brief names %v, want %v", named, want)
	}
	for _, f := range named {
		if _, err := os.Stat(filepath.Join(run, f)); err != nil {
			t.Errorf("brief names %s, not in the run dir", f)
		}
	}
}
