package dun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

func startFiles(t *testing.T, cfg Config) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir()) // never the user's servers or config
	if cfg.Workspace == "" {
		cfg.Workspace = t.TempDir()
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func fileToolNames(h *Harness) []string {
	var out []string
	for _, n := range h.ToolNames() {
		if strings.HasPrefix(n, "file_") {
			out = append(out, n)
		}
	}
	return out
}

func dispatchFile(t *testing.T, h *Harness, name, args string) string {
	t.Helper()
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = name, args
	out, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFileTools_Config(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		files    *FilesConfig
		want     []string
		inSystem []string
		notSys   []string
	}{
		{"defaults", nil,
			[]string{"file_edit", "file_grep", "file_list", "file_read", "file_write"},
			[]string{"file_read, file_edit, file_write, file_grep, file_list:", "Prefer them to exec", "editing (not sed"}, nil},
		{"off", &FilesConfig{Tools: []string{}}, nil, nil, []string{"file_read"}},
		{"read-only", &FilesConfig{ReadOnly: &yes},
			[]string{"file_grep", "file_list", "file_read"},
			[]string{"file_read, file_grep, file_list:"}, []string{"editing (not sed", "file_edit"}},
		{"read-only false is the default", &FilesConfig{ReadOnly: &no},
			[]string{"file_edit", "file_grep", "file_list", "file_read", "file_write"}, nil, nil},
		{"delete and move opt in", &FilesConfig{Tools: []string{"read", "delete", "move"}},
			[]string{"file_delete", "file_move", "file_read"}, nil, []string{"file_grep"}},
		{"unknown tool turns them off, not the session", &FilesConfig{Tools: []string{"chmod"}}, nil, nil, []string{"file_read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := startFiles(t, Config{Files: c.files})
			if got := fileToolNames(h); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("file tools = %v, want %v", got, c.want)
			}
			for _, w := range c.inSystem {
				if !strings.Contains(h.Session.System, w) {
					t.Errorf("system prompt lacks %q", w)
				}
			}
			for _, w := range c.notSys {
				if strings.Contains(h.Session.System, w) {
					t.Errorf("system prompt mentions %q", w)
				}
			}
		})
	}
}

// The tools are rooted at the session's working tree and follow it when the
// session moves (a worktree switch is a Rehoist).
func TestFileTools_RootFollowsWorkingTree(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(first, "a.txt"), []byte("first\n"), 0o644))
	must(os.WriteFile(filepath.Join(second, "a.txt"), []byte("second\n"), 0o644))

	var reported []string
	h := startFiles(t, Config{Workspace: first, OnToolCall: func(tool string, args map[string]any, result string) {
		reported = append(reported, tool+" "+args["path"].(string))
	}})
	if out := dispatchFile(t, h, "file_read", `{"path":"a.txt"}`); !strings.Contains(out, "1\tfirst") {
		t.Fatalf("read in first root: %s", out)
	}
	if out := dispatchFile(t, h, "file_read", `{"path":"../`+filepath.Base(second)+`/a.txt"}`); !strings.HasPrefix(out, "ERROR") {
		t.Fatalf("a sibling directory was readable: %s", out)
	}
	ft := h.files
	h.applyTools() // an unrelated rebuild keeps the toolset, and what it has read
	if h.files != ft {
		t.Fatal("rebuild replaced the toolset for the same root")
	}
	if out := dispatchFile(t, h, "file_write", `{"path":"a.txt","content":"x\n"}`); strings.HasPrefix(out, "ERROR") {
		t.Fatalf("read-tracking lost across a rebuild: %s", out)
	}

	h.Rehoist(second, nil, false)
	if out := dispatchFile(t, h, "file_read", `{"path":"a.txt"}`); !strings.Contains(out, "1\tsecond") {
		t.Fatalf("read after rehoist: %s", out)
	}
	if got := h.files.Root(); got != mustEval(t, second) {
		t.Fatalf("root = %s, want %s", got, second)
	}
	if len(reported) < 3 || reported[0] != "file_read a.txt" {
		t.Fatalf("OnToolCall saw %v", reported)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFileTools_RootIsTheWorktree(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := t.TempDir()
	mustRunGit(t, repo, "init", "-q", "-b", "main")
	mustRunGit(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "only-in-checkout.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, isRepo, err := NewWorktree(repo, nil)
	if err != nil || !isRepo {
		t.Fatalf("worktree: %v (repo=%v)", err, isRepo)
	}
	t.Cleanup(wt.Cleanup)
	h := startFiles(t, Config{Workspace: wt.Path, Worktree: wt})
	if got := h.files.Root(); got != mustEval(t, wt.Path) {
		t.Fatalf("root = %s, want the worktree %s", got, wt.Path)
	}
	out := dispatchFile(t, h, "file_read", `{"path":"only-in-checkout.txt"}`)
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("an untracked file of the main checkout was readable from the worktree: %s", out)
	}
	out = dispatchFile(t, h, "file_write", `{"path":"new.txt","content":"hi\n"}`)
	if _, err := os.Stat(filepath.Join(wt.Path, "new.txt")); err != nil {
		t.Fatalf("write did not land in the worktree: %s", out)
	}
}

func TestLoadFiles_Layers(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if LoadFiles(dir) != nil {
		t.Fatal("no section anywhere must be nil (the defaults)")
	}
	write(ProjectServersFile, `{"files":{"tools":["read","grep"],"read_only":true,"read_max":1000,"ignore":["gen/"]}}`)
	write(LocalServersFile, `{"files":{"read_only":false,"ignore":["tmp/"]}}`)
	c := LoadFiles(dir)
	if c == nil {
		t.Fatal("nil")
	}
	if !reflect.DeepEqual(c.Tools, []string{"read", "grep"}) {
		t.Errorf("tools = %v (an unstated field must be inherited)", c.Tools)
	}
	if c.ReadOnly == nil || *c.ReadOnly {
		t.Errorf("read_only: the local layer's false must win")
	}
	if c.ReadMax != 1000 {
		t.Errorf("read_max = %d", c.ReadMax)
	}
	if !reflect.DeepEqual(c.Ignore, []string{"gen/", "tmp/"}) {
		t.Errorf("ignore = %v, want both layers", c.Ignore)
	}

	write(LocalServersFile, `{"files":{"tools":[]}}`)
	c = LoadFiles(dir)
	if c.Tools == nil || len(c.Tools) != 0 {
		t.Fatalf("tools: [] must mean off, got %#v", c.Tools)
	}
	if _, ok := c.toolsetConfig(dir); ok {
		t.Fatal("tools: [] did not turn the file tools off")
	}
	write(LocalServersFile, `{"files":{"tools":null}}`)
	if c = LoadFiles(dir); !reflect.DeepEqual(c.Tools, []string{"read", "grep"}) {
		t.Fatalf("tools: null must inherit, got %v", c.Tools)
	}
}
