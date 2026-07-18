package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Isolation, tier 2 — the exec tool.
//
// None of the three MCP servers runs arbitrary commands (mcpshell is sandboxed;
// poly-lsp only gives diagnostics), but a coding agent must build/test/git. exec
// is that command-runner. The DANGEROUS part — running model-authored commands —
// is contained by a Docker container (DockerExec); HostExec is the escape hatch
// for a trusted/throwaway environment. Either way the command runs against the
// session's worktree, so it sees the agent's edits.

// ExecBackend runs a shell command against the workspace and returns combined
// stdout+stderr. A non-zero exit is reported in the output (for the model), not
// as a Go error.
type ExecBackend interface {
	Run(ctx context.Context, command string) string
}

// HostExec runs commands on the host, in dir. Use only for a trusted or
// throwaway workspace — there is no sandbox.
type HostExec struct{ Dir string }

func (h HostExec) Run(ctx context.Context, command string) string {
	cmd := exec.CommandContext(ctx, "sh", "-lc", command)
	cmd.Dir = h.Dir
	return finish(cmd)
}

// DockerExec runs each command in a fresh container of Image with Dir mounted at
// /work (the cwd). The container is the sandbox: model-authored commands can't
// touch the host, only the mounted worktree.
type DockerExec struct {
	Dir   string
	Image string
	// Network, if false, runs with --network none (no egress). Default false.
	Network bool
}

func (d DockerExec) Run(ctx context.Context, command string) string {
	args := []string{"run", "--rm", "-v", d.Dir + ":/work", "-w", "/work"}
	if !d.Network {
		args = append(args, "--network", "none")
	}
	args = append(args, d.Image, "sh", "-lc", command)
	return finish(exec.CommandContext(ctx, "docker", args...))
}

func finish(cmd *exec.Cmd) string {
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += fmt.Sprintf("[exit: %v]", err)
	}
	return s
}

// execToolDef is the tool the model calls to run commands.
func execToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "exec"
	td.Function.Description = "Run a shell command (build, test, git, ls, …) in the workspace. " +
		"Returns combined stdout+stderr; a non-zero exit is shown as [exit: …]. Use this to " +
		"verify edits (build/test) and to run git. For a LONG command (the full test suite, a " +
		"build), set background:true and keep working — you'll get a notification when it finishes."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":    map[string]any{"type": "string", "description": "the shell command to run"},
			"background": map[string]any{"type": "boolean", "description": "run asynchronously; get notified when it finishes"},
		},
		"required": []string{"command"},
	}
	return td
}

// withExec wraps a dispatcher so the built-in "exec" tool is handled locally:
// synchronous by default, or async via startBg when background:true (its
// completion arrives later as a notification). Everything else routes to MCP.
func withExec(inner agent.ToolDispatcher, backend ExecBackend, onCall func(string, map[string]any, string), startBg func(command string) int) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "exec" {
			return inner(ctx, tc)
		}
		var args struct {
			Command    string `json:"command"`
			Background bool   `json:"background"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if strings.TrimSpace(args.Command) == "" {
			return "ERROR: exec requires a non-empty command", nil
		}
		if args.Background && startBg != nil {
			id := startBg(args.Command)
			res := fmt.Sprintf("Started background job #%d: `%s`. It runs in the sandbox; you'll be "+
				"notified when it finishes. Continue with other work in the meantime.", id, args.Command)
			if onCall != nil {
				onCall("exec", map[string]any{"command": args.Command, "background": true}, res)
			}
			return res, nil
		}
		out := backend.Run(ctx, args.Command)
		if onCall != nil {
			onCall("exec", map[string]any{"command": args.Command}, out)
		}
		return out, nil
	}
}
