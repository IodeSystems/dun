package main

import (
	"strings"

	"github.com/iodesystems/dun/internal/cmdstats"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// recordSlash counts a slash line the engine routes (cmdstats): the command's
// name, when the engine lists it, never its arguments. A command the TUI ran
// itself is counted there, so a line is counted once.
func recordSlash(h *harness.Harness, line string) {
	recordSlashIn(line, commandsEvent(h).Items)
}

func recordSlashIn(line string, known []proto.CommandSpec) {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if len(f) == 0 {
		return
	}
	name := strings.ToLower(f[0])
	if name == "poly-lsp" { // routeSlash's alias for /lsp
		name = "lsp"
	}
	for _, c := range known {
		if c.Name == name {
			cmdstats.Record(name, cmdstats.SurfaceEngine)
			return
		}
	}
}
