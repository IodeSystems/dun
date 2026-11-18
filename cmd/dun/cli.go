package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/term"
)

// subcommand is a word dun takes in the first position.
type subcommand struct{ name, help string }

// subcommands, in --help order. A bare word that is not one of them is
// refused (USER 2026-11-17): a task runs headless with -p only. completion.go
// completes from this list.
var subcommands = []subcommand{
	{"doctor", "check providers, tools, dund and the service"},
	{"tools", "the tools against the versions this dun pins"},
	{"tmux", "the tmux.conf lines that show dun's state"},
	{"mcp", "dun's sessions as an MCP server on stdio"},
	{"watch", "a line each time a session ends a turn or asks"},
	{"msg", "message dun sessions from a shell"},
	{"session", "what dun is in a session, as JSON"},
	{"spend", "what dun has spent"},
	{"oauth", "an OAuth MCP server's bearer token"},
	{"dund", "the session service"},
}

func isSubcommand(w string) bool {
	return slices.ContainsFunc(subcommands, func(c subcommand) bool { return c.name == w })
}

// headlessTask is the task the command line gives, after flag.Parse:
//
//   - --engine: the words are its first task (none is fine).
//   - -p: the words are the task; with none, stdin is the task when it is not
//     a terminal. answer is true: the run prints the answer alone on stdout.
//   - neither: there must be no words. A bare word is an unknown command.
//
// underDund: a dund from before --engine spawns `dun -p` with DUN_DUND set and
// no task. That is still the engine, or an upgrade would turn every running
// dund's sessions into headless runs that read their JSON as a task.
func headlessTask(args []string, headless bool, engine *bool, stdin io.Reader, tty, underDund bool) (task string, answer bool, err error) {
	task = strings.TrimSpace(strings.Join(args, " "))
	switch {
	case headless && *engine:
		return "", false, errors.New("-p (one task headless) and --engine (the JSON protocol) are two modes: give one")
	case *engine:
		return task, false, nil
	case !headless:
		if len(args) == 0 {
			return "", false, nil
		}
		return "", false, unknownCommand(args)
	case task != "":
		return task, true, nil
	case underDund:
		*engine = true
		return "", false, nil
	case tty:
		return "", false, errors.New(`-p needs a task: dun -p "fix the flaky test", or on stdin: echo "fix the flaky test" | dun -p`)
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", false, fmt.Errorf("-p: read the task from stdin: %w", err)
	}
	if task = strings.TrimSpace(string(b)); task == "" {
		return "", false, errors.New(`-p read no task from stdin: dun -p "fix the flaky test"`)
	}
	return task, true, nil
}

// unknownCommand is the refusal for bare words on the command line.
func unknownCommand(args []string) error {
	if isSubcommand(args[0]) {
		return fmt.Errorf("%q is a command: give it first, before any flag: dun %s …", args[0], args[0])
	}
	return fmt.Errorf("unknown command %q — for a headless task: dun -p %q", args[0], strings.Join(args, " "))
}

func stdinIsTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
