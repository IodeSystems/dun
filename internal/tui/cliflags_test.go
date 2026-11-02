package tui

import (
	"slices"
	"testing"
)

// The flags reach the engine's argv, and dund's argv filter, for the TUI.
func TestFlags_PassToTheEngine(t *testing.T) {
	o := TuiOpts{Workspace: "/ws", Suggest: true, Rag: "false", Lsp: "false", NoTools: true}
	for _, mode := range []string{"-p", "--tui"} {
		args := ProcArgs(o, mode)
		for _, want := range []string{"--no-tools", "--rag=false", "--lsp=false"} {
			if !slices.Contains(args, want) {
				t.Errorf("%s argv lacks %s: %v", mode, want, args)
			}
		}
	}
	eng := engineArgs(o)
	if !slices.Contains(eng, "--no-tools") || !slices.Contains(eng, "--rag=false") {
		t.Errorf("dund engine args: %v", eng)
	}
	if slices.Contains(ProcArgs(TuiOpts{Workspace: "/ws", Suggest: true}, "-p"), "--no-tools") {
		t.Error("--no-tools without being asked")
	}
}
