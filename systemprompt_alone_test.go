package dun

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// The pass-through worktree dun makes without --worktree is the directory
// itself: the prompt must not call it an isolated worktree. A real one (its
// own branch) still says so.
func TestSystemFor_ContainmentOnlyWhenIsolated(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}
	inPlace := systemFor(tools, HostExec{Dir: "/w"}, &Worktree{Path: "/w"}, "", false)
	if strings.Contains(inPlace, "worktree") || strings.Contains(inPlace, "isolated") {
		t.Errorf("in place, the prompt claims isolation: %s", inPlace)
	}
	real := systemFor(tools, HostExec{Dir: "/w"}, &Worktree{Path: "/wt", Branch: "dun/1"}, "", false)
	if !strings.Contains(real, "Containment: git worktree at /wt") {
		t.Errorf("a real worktree is not named: %s", real)
	}
}

// --non-interactive leaves out the thinking headings, which only the TUI's thought line
// uses.
func TestSystemFor_BareDropsThinkingHeadings(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}
	if !strings.Contains(systemFor(tools, nil, nil, "", false), "# Thinking:") {
		t.Error("a client-facing run lost the thinking headings")
	}
	if strings.Contains(systemFor(tools, nil, nil, "", true), "# Thinking:") {
		t.Error("--non-interactive still asks for thinking headings")
	}
}

// Every run's work line says to finish, install what is missing, and check
// the task's requirements; --non-interactive adds that the model is alone.
func TestSystemFor_FinishAlwaysAloneOnlyNonInteractive(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}
	for _, alone := range []bool{false, true} {
		s := systemFor(tools, nil, nil, "", alone)
		if !strings.Contains(s, "check each requirement it states") {
			t.Errorf("alone=%v: no finish-and-check line", alone)
		}
		if strings.Contains(s, "You are on your own") != alone {
			t.Errorf("alone=%v: on-your-own line present=%v", alone, !alone)
		}
	}
}

// What --non-interactive is, end to end: no ask_user (no Ask), no thinking headings or
// Markdown note, and the {OUTPUT} note KEPT, since the engine expands slots
// and a script gets the output without the model retyping it.
func TestBare_KeepsOutputSlotsDropsClientNotes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), Servers: []Server{}, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sys := h.Session.System
	if !strings.Contains(sys, "{OUTPUT}") {
		t.Error("--non-interactive dropped the {OUTPUT} note")
	}
	for _, gone := range []string{"# Thinking:", "shown as Markdown"} {
		if strings.Contains(sys, gone) {
			t.Errorf("--non-interactive kept %q", gone)
		}
	}
	if slices.Contains(h.ToolNames(), "ask_user") {
		t.Error("ask_user listed with no Ask")
	}
}

// When to recap is said once, in the system prompt, and only while recap is
// a tool: switched off, the line goes with it. The tool's own description no
// longer argues against it.
func TestSystemRecap_WithTheToolOnly(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []Server{}, Exec: &HostShell{Dir: ws}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !strings.Contains(h.Session.System, "While work remains in a turn, recap") {
		t.Fatal("no recap line in the system prompt")
	}
	if strings.Contains(recapToolDef().Function.Description, "not recommended") {
		t.Error("the recap description still argues against it")
	}
	if _, err := h.SetServerOn(context.Background(), "dun:recap", ScopeProject, ptr(false)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Session.System, "recap what is settled") {
		t.Error("the recap line outlived the tool")
	}
}
