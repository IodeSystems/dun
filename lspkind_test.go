package dun

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// fakeLSPList stands in for `poly-lsp-mcp lsp list|add|remove`.
func fakeLSPList(t *testing.T, calls *[][]string) {
	t.Helper()
	old := runLSPCLI
	runLSPCLI = func(_ context.Context, command string, _ []string, _ string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{command}, args...))
		if args[0] != "list" {
			return []byte("added zig\n"), nil
		}
		return []byte(`{"languages":[
			{"name":"go","extensions":["go"],"source":"builtin","lsp":{"name":"gopls","found":true,"path":"/bin/gopls"}},
			{"name":"python","extensions":["py"],"source":"builtin","lsp":{"name":"pylsp","found":false},"install":{"cmd":"pipx install python-lsp-server"}},
			{"name":"java","extensions":["java"],"source":"user","lsp":{"name":"jdtls","found":true},"offByDefault":true,"formatOnEdit":true}
		]}`), nil
	}
	t.Cleanup(func() { runLSPCLI = old })
}

func lspHarness(t *testing.T) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: t.TempDir() + "/s.jsonl",
		Servers: []Server{{ID: ServerCode, Command: "poly-lsp-mcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// /lsp's table is poly-lsp-mcp's language list: a language off by default
// is off until a scope turns it on, and the switches become the code
// server's --lsp-off / --lsp-on.
func TestLSPKind_TableAndFlags(t *testing.T) {
	var calls [][]string
	fakeLSPList(t, &calls)
	h := lspHarness(t)

	if got := h.codeLSPArgs(); got != nil || len(calls) != 0 {
		t.Fatalf("no switches: args %q, %d lsp runs", got, len(calls))
	}
	v, ok := h.SwitchView(lspKind)
	if !ok || len(v.Items) != 3 {
		t.Fatalf("view: %+v", v)
	}
	byName := map[string]SwitchState{}
	for _, it := range v.Items {
		byName[it.Name] = it
	}
	if !byName["go"].On || !byName["python"].On || byName["java"].On {
		t.Errorf("defaults: %+v", v.Items)
	}
	if d := byName["python"].Description; !strings.Contains(d, "not installed (pipx install python-lsp-server)") || !strings.Contains(d, ".py") {
		t.Errorf("python row: %q", d)
	}
	if s := byName["java"].Source; s != "user · off by default · format on edit" {
		t.Errorf("java source: %q", s)
	}

	off, on := false, true
	if _, err := h.SetSwitch(lspKind, "go", ScopeProject, &off); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetSwitch(lspKind, "java", ScopeSession, &on); err != nil {
		t.Fatal(err)
	}
	if got, want := h.codeLSPArgs(), []string{"--lsp-off", "go", "--lsp-on", "java"}; !slices.Equal(got, want) {
		t.Errorf("args %q, want %q", got, want)
	}
	if _, err := h.SetSwitch(lspKind, "rust", ScopeSession, &on); err == nil || !strings.Contains(err.Error(), "go, python, java") {
		t.Errorf("an unknown language: %v", err)
	}
	if _, err := h.SetSwitch(lspKind, "go", ScopeProject, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := h.codeLSPArgs(), []string{"--lsp-on", "java"}; !slices.Equal(got, want) {
		t.Errorf("after unset: %q, want %q", got, want)
	}
}

// /lsp add and remove are poly-lsp-mcp's own commands, run as given.
func TestLSPKind_AddRunsPolyLSP(t *testing.T) {
	var calls [][]string
	fakeLSPList(t, &calls)
	h := lspHarness(t)
	msg, err := h.LSPEdit(context.Background(), "add", []string{"zig", "--ext", ".zig", "--cmd", "zls"})
	if err != nil || msg != "added zig" {
		t.Fatalf("add: %q %v", msg, err)
	}
	if want := []string{"poly-lsp-mcp", "add", "zig", "--ext", ".zig", "--cmd", "zls"}; !slices.Equal(calls[len(calls)-1], want) {
		t.Errorf("ran %q, want %q", calls[len(calls)-1], want)
	}
	if _, err := h.LSPEdit(context.Background(), "remove", nil); err == nil {
		t.Error("remove with no name ran")
	}
}
