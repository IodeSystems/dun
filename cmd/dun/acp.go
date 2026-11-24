package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/iodesystems/dun/internal/acp"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/tui"
)

// `dun acp`: dun as an Agent Client Protocol agent on stdio, for editors
// (Zed, JetBrains, Neovim). A client of dund, as the TUI is
// (internal/acp, docs/acp.md).

const acpHelp = `dun acp — dun as an Agent Client Protocol (ACP) agent, on stdin/stdout

An editor starts it and drives dun sessions through it: prompts, the
reply as it streams, tool calls with their diffs, the plan, dun's slash
commands, and dun's questions as permission prompts. It talks to dund over
its unix socket, as the TUI does, and starts dund when nothing is
listening. A session it starts is an ordinary dun session in the editor's
project directory, worked in place (no worktree): dun --resume <id> and
the session list open it too.

Zed (settings.json):

  "agent_servers": {
    "dun": {"type": "custom", "command": "dun", "args": ["acp"], "env": {}}
  }

JetBrains (~/.jetbrains/acp.json; the command is an absolute path):

  {"agent_servers": {"dun": {"command": "/home/you/.local/bin/dun", "args": ["acp"]}}}

Not supported: the editor's MCP servers (dun runs its own, from dun.json),
the editor's file system and terminals, modes, images.

Flags:

  --debug     log the protocol's diagnostics (stderr)
  -h, --help  this text

docs/acp.md has the details.
`

func acpCmd(args []string, in io.Reader, out, errw io.Writer) int {
	level := slog.LevelInfo
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Fprint(out, acpHelp)
			return 0
		case "--debug":
			level = slog.LevelDebug
		default:
			fmt.Fprintf(errw, "dun acp: no flag %q\n\n%s", a, acpHelp)
			return 2
		}
	}
	// stdout is the protocol: every log line goes to stderr, which an
	// editor keeps in its agent log.
	log := slog.New(slog.NewTextHandler(errw, &slog.HandlerOptions{Level: level}))
	if err := acp.Serve(in, out, tui.DialDund, log, buildinfo.Version); err != nil {
		fmt.Fprintln(errw, "dun acp:", err)
		return 1
	}
	return 0
}
