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
	{"acp", "dun as an Agent Client Protocol agent, for editors"},
	{"watch", "a line each time a session ends a turn or asks"},
	{"msg", "message dun sessions from a shell"},
	{"session", "what dun is in a session, as JSON"},
	{"spend", "what dun has spent"},
	{"errors", "the turn errors dun kept, for a report"},
	{"trace", "find a request by its trace id"},
	{"ps", "every process dun runs, by session: CPU, memory, IO"},
	{"oauth", "an OAuth MCP server's bearer token"},
	{"dund", "the session service"},
	{"completion", "the shell completion script: bash, zsh or fish"},
}

func isSubcommand(w string) bool {
	return slices.ContainsFunc(subcommands, func(c subcommand) bool { return c.name == w })
}

// cmdLine is what headlessTask reads besides the parsed flags.
type cmdLine struct {
	stdin     io.Reader
	tty       bool      // stdin is a terminal
	oldDund   bool      // spawned the way a dund from before --engine spawns (oldDundSpawn)
	afterDash bool      // the words came after a "--"
	notice    io.Writer // where to say stdin is being read; nil: say nothing
}

// headlessTask is the task the command line gives, after flag.Parse:
//
//   - --engine: the words are its first task (none is fine).
//   - -p: the words are the task; with none, stdin is the task when it is not
//     a terminal. answer is true: the run prints the answer alone on stdout.
//   - neither: there must be no words. A bare word is an unknown command.
func headlessTask(args []string, headless bool, engine *bool, c cmdLine) (task string, answer bool, err error) {
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
		return "", false, unknownCommand(args, c.afterDash)
	case task != "":
		return task, true, nil
	case c.oldDund:
		*engine = true
		return "", false, nil
	case c.tty:
		return "", false, errors.New(`-p needs a task: dun -p "fix the flaky test", or on stdin: echo "fix the flaky test" | dun -p`)
	}
	if c.notice != nil {
		fmt.Fprintln(c.notice, "dun: reading the task from stdin… (ctrl+d ends it)")
	}
	b, err := io.ReadAll(c.stdin)
	if err != nil {
		return "", false, fmt.Errorf("-p: read the task from stdin: %w", err)
	}
	if task = strings.TrimSpace(string(b)); task == "" {
		return "", false, errors.New(`-p read no task from stdin: dun -p "fix the flaky test"`)
	}
	return task, true, nil
}

// oldDundSpawn reports whether this process was started exactly as a dund
// from before --engine (USER 2026-11-17) starts an engine: argv
// `dun -p --workspace <ws> [engine flags…]`, DUN_CHILD=1 and DUN_DUND=1 in
// the environment, stdin a pipe. That is still the engine, or an upgrade
// would turn a running old dund's sessions into headless runs that read
// their JSON as a task. Remove after one release past the rename: by then
// every dund in use spawns --engine.
func oldDundSpawn(argv []string, getenv func(string) string, tty bool) bool {
	return len(argv) >= 4 && argv[1] == "-p" && argv[2] == "--workspace" &&
		getenv("DUN_CHILD") == "1" && getenv("DUN_DUND") == "1" && !tty
}

// unknownCommand is the refusal for bare words on the command line.
func unknownCommand(args []string, afterDash bool) error {
	hint := fmt.Sprintf("for a headless task: dun -p %q", strings.Join(args, " "))
	if afterDash {
		return fmt.Errorf("%q after -- is not a task either — %s", args[0], hint)
	}
	if isSubcommand(args[0]) {
		return fmt.Errorf("%q is a command: give it first, before any flag: dun %s …", args[0], args[0])
	}
	return fmt.Errorf("unknown command %q — %s", args[0], hint)
}

// dashBeforeArgs reports whether a "--" ended the flags in argv, given the
// words flag.Parse left.
func dashBeforeArgs(argv []string, rest []string) bool {
	i := len(argv) - len(rest) - 1
	return len(rest) > 0 && i >= 1 && argv[i] == "--"
}

func stdinIsTTY() bool  { return term.IsTerminal(int(os.Stdin.Fd())) }
func stderrIsTTY() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
