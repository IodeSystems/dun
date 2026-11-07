package dun

import (
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

// --bare leaves out the thinking headings, which only the TUI's thought line
// uses.
func TestSystemFor_BareDropsThinkingHeadings(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}
	if !strings.Contains(systemFor(tools, nil, nil, "", false), "# Thinking:") {
		t.Error("a client-facing run lost the thinking headings")
	}
	if strings.Contains(systemFor(tools, nil, nil, "", true), "# Thinking:") {
		t.Error("--bare still asks for thinking headings")
	}
}
