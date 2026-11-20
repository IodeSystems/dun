package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// usage prints what dun is and how to start, then the flags by group, as
// --long-flags. The advanced group is in `dun --help-all` only.
//
// Go's flag package accepts -tui and --tui identically but PRINTS the first,
// and `-tui` reads like the three short flags -t -u -i. Only genuinely
// single-letter flags keep one dash.
func usage() { writeUsage(os.Stderr, flag.CommandLine, helpWidth(), false) }

// flagGroups orders --help; a flag in none of them is listed under the last.
// The last (advanced) is printed by --help-all only.
var flagGroups = []struct {
	title string
	names []string
}{
	{"start and sessions", []string{"setup", "continue", "resume", "new", "sessions", "tui", "p", "version"}},
	{"model and provider", []string{"url", "model", "key", "child-model", "child-provider", "child-effort", "tier-fast", "tier-work", "tier-think", "default-tier", "timeout", "timeout-grace", "no-suggest", "rephrase", "bare"}},
	{"workspace and isolation", []string{"workspace", "no-dir", "worktree", "docker", "docker-network", "rag", "lsp", "no-tools", "media"}},
	{"experimental", []string{"exp-iterm-tabcontrol"}},
	{"advanced", []string{"engine", "listen", "persist", "disable-exit", "pick", "replay", "replay-speed", "input-delay", "max-gap"}},
}

// helpStart is the "start:" block: a command and what it does.
var helpStart = [][2]string{
	{"cd your-project && dun", "open dun there, then /login (enter on the empty prompt opens it) to connect an AI service, and ask"},
	{`dun -p "fix the test"`, "run one task headless: the answer on stdout, the work and the diff on stderr; with no task, the task is read from stdin"},
	{"dun --continue", "reopen the most recent session for this workspace"},
	{"dun doctor [--fix]", "check providers, tools, dund and the service; --fix runs what it can"},
	{"dun tools [--upgrade]", "the tools against the versions this dun pins; --upgrade installs them (dun tools --help)"},
	{"dun tmux", "the tmux.conf lines that show dun's state in the window list"},
	{"dun mcp", "dun's sessions as an MCP server on stdio, for another agent (dun mcp --help)"},
	{"dun watch <session>", "a line each time a session ends a turn or asks (dun watch --help)"},
	{"dun msg", "message dun sessions and listen for messages from a shell (dun msg --help)"},
	{"dun errors [--json]", "the turn errors dun kept, newest last, with the endpoint and the server's body; to paste in a report (dun errors --help)"},
	{"dun trace <id>", "find a request by the trace id in a provider's or gateway's log: its session, role and every line logged for it"},
	{"dun ps [--watch]", "every process dun runs, by session: CPU, memory, IO; SESSION (none) is a leak (dun ps --help)"},
	{"dun --engine", "engine mode: line-delimited JSON events on stdin/stdout (what dund runs)"},
	{"dun dund …", "the session service (dun dund --help)"},
	{"dun completion [shell]", "shell completion: source <(dun completion bash), or zsh, fish"},
}

// helpWidth is the terminal's width (stderr, where --help goes), else
// $COLUMNS, else 80.
func helpWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 0 {
		return w
	}
	return 80
}

// wrapText breaks s into lines of at most width runes, at spaces; a word
// longer than width is a line of its own.
func wrapText(s string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = w
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// writeIndented writes s wrapped to width with every line after indent.
func writeIndented(w io.Writer, s string, indent, width int) {
	pad := strings.Repeat(" ", indent)
	for _, l := range wrapText(s, max(width-indent, 20)) {
		fmt.Fprintln(w, pad+l)
	}
}

// flagDefault is " (default X)", or "" when X is the zero value or the text
// already says the default.
func flagDefault(f *flag.Flag, help string) string {
	switch f.DefValue {
	case "", "false", "0", "0s", "[]":
		return ""
	}
	if strings.Contains(help, "(default") {
		return ""
	}
	return " (default " + f.DefValue + ")"
}

// writeUsage is --help (all: --help-all) for fs, wrapped to width. A flag is
// its name and argument type on one line, its text on the next, indented;
// --x and --no-x are one entry.
func writeUsage(w io.Writer, fs *flag.FlagSet, width int, all bool) {
	writeIndented(w, "dun — a coding assistant in your terminal: it works a task in your project.", 0, width)
	fmt.Fprintln(w, "\nstart:")
	const col = 26 // "  " + the widest command + a space
	for _, e := range helpStart {
		if width < 60 { // no room for two columns
			fmt.Fprintln(w, "  "+e[0])
			writeIndented(w, e[1], 6, width)
			continue
		}
		lines := wrapText(e[1], width-col)
		fmt.Fprintf(w, "  %-*s%s\n", col-2, e[0], lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintln(w, strings.Repeat(" ", col)+l)
		}
	}
	fmt.Fprintln(w)
	writeIndented(w, "DUN_URL, DUN_MODEL, DUN_LLM_KEY in the environment set the provider without /login (--url, --model, --key for one run).", 2, width)

	entry := func(f *flag.Flag) {
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		name, help := flag.UnquoteUsage(f)
		head := "  " + dash + f.Name
		if fs.Lookup("no-"+f.Name) != nil {
			head += ", --no-" + f.Name
		}
		if name != "" {
			head += " " + name
		}
		fmt.Fprintln(w, head)
		writeIndented(w, help+flagDefault(f, help), 6, width)
	}
	hidden := map[string]bool{"help-all": true}
	for n := range removedFlags {
		hidden[n] = true // accepted, not listed
	}
	fs.VisitAll(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "no-") && fs.Lookup(strings.TrimPrefix(f.Name, "no-")) != nil {
			hidden[f.Name] = true // in its --x entry
		}
	})
	grouped := map[string]bool{}
	for _, g := range flagGroups {
		for _, n := range g.names {
			grouped[n] = true
		}
	}
	for i, g := range flagGroups {
		last := i == len(flagGroups)-1
		if last && !all {
			break
		}
		var fl []*flag.Flag
		for _, n := range g.names {
			if f := fs.Lookup(n); f != nil && !hidden[n] {
				fl = append(fl, f)
			}
		}
		if last {
			fs.VisitAll(func(f *flag.Flag) {
				if !grouped[f.Name] && !hidden[f.Name] {
					fl = append(fl, f)
				}
			})
		}
		if len(fl) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, f := range fl {
			entry(f)
		}
	}
	if !all {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "more flags: dun --help-all")
	}
}
