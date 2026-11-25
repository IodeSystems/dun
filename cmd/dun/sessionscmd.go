package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/iodesystems/dun/internal/sesslist"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/proto"
)

// `dun sessions`: the sessions of this workspace (--all: every workspace),
// newest first, one line each: id, directory, status, age, last request.
//
// Live state comes from dund's session list (the call the TUI's picker
// makes). dund reports whether a session's engine runs, not what the engine
// is doing, so the status is running / stopped / saved; with dund down every
// session is saved. The last request is read from the end of the transcript
// (harness.LastAsk), and only for the rows shown.

const sessionsHelp = `dun sessions — the sessions dun keeps, newest first

  dun sessions [--all] [-n N] [--json] [--workspace DIR]

One line per session: id, [directory], status, age, last request.

  --all            every workspace's sessions (adds the directory column)
  -n N             at most N rows (default 20; 0: all)
  --json           {"sessions": [...], "total": N, "dund": bool} for scripts (total: sessions found, not all checked)
  --workspace DIR  another workspace than the current directory

Status: running (dund holds a live engine), stopped (dund knows the
session, no engine), saved (a file only; every session when dund is down).
Age is the last activity. The request is the last thing you asked, cut to
the terminal width.

dun --sessions is the same as dun sessions.`

const sessionsDefaultN = 20

type sessionsOpts struct {
	ws   string // the workspace, absolute
	all  bool
	n    int // rows; 0 = all
	json bool
	// dund lists dund's sessions; nil: tui.DundSessions.
	dund func() ([]proto.SessionSummary, error)
	now  time.Time // zero: time.Now()
	// width of the terminal; 0: read it.
	width int
}

func sessionsCmd(args []string, out, errw io.Writer) int {
	o := sessionsOpts{n: sessionsDefaultN}
	ws := "."
	for i := 0; i < len(args); i++ {
		name, inline, hasInline := strings.Cut(args[i], "=")
		// value is the flag's argument: after "=", else the next word.
		value := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch name {
		case "-h", "--help", "help":
			fmt.Fprintln(out, sessionsHelp)
			return 0
		case "--all":
			o.all = true
		case "--json":
			o.json = true
		case "-n":
			v, ok := value()
			n, err := strconv.Atoi(v)
			if !ok || err != nil || n < 0 {
				fmt.Fprintln(errw, "dun sessions: -n needs a number of rows (0: all)")
				return 2
			}
			o.n = n
		case "--workspace":
			v, ok := value()
			if !ok || v == "" {
				fmt.Fprintln(errw, "dun sessions: --workspace needs a directory")
				return 2
			}
			ws = v
		default:
			fmt.Fprintf(errw, "dun sessions: unknown argument %q\n\n%s\n", args[i], sessionsHelp)
			return 2
		}
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		fmt.Fprintln(errw, "dun sessions:", err)
		return 1
	}
	o.ws = abs
	return sessionsRun(o, out, errw)
}

func sessionsRun(o sessionsOpts, out, errw io.Writer) int {
	if o.dund == nil {
		o.dund = tui.DundSessions
	}
	if o.now.IsZero() {
		o.now = time.Now()
	}
	rows, total, dundUp := sesslist.Collect(sesslist.Opts{WS: o.ws, All: o.all, N: o.n, Dund: o.dund})
	if o.json {
		if rows == nil {
			rows = []sesslist.Row{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"sessions": rows, "total": total, "dund": dundUp}); err != nil {
			fmt.Fprintln(errw, "dun sessions:", err)
			return 1
		}
		return 0
	}
	if !dundUp {
		fmt.Fprintln(errw, "dun sessions: dund is not running; every session shows as saved")
	}
	if len(rows) == 0 {
		fmt.Fprintln(errw, "dun: no saved sessions for this workspace")
		return 0
	}
	if o.width == 0 {
		o.width = sessionsWidth(out)
	}
	renderSessions(out, rows, total, o)
	return 0
}

func sessionsWidth(out io.Writer) int {
	if f, ok := out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return w
		}
	}
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 0 {
		return w
	}
	return 80
}

func renderSessions(out io.Writer, rows []sesslist.Row, total int, o sessionsOpts) {
	const dirW = 28
	idW := 0
	for _, r := range rows {
		idW = max(idW, len(r.ID))
	}
	for _, r := range rows {
		line := textfmt.Pad(r.ID, idW)
		if o.all {
			line += "  " + textfmt.Pad(sesslist.ClipLeft(sesslist.TildeDir(r.Workspace), dirW), dirW)
		}
		line += "  " + textfmt.Pad(r.Status, 7) + "  " + textfmt.Pad(sesslist.Age(r.LastActive, o.now), 8) + "  "
		ask := textfmt.OneLine(r.Last)
		if ask == "" {
			ask = "-"
		}
		room := max(o.width-len([]rune(line)), 10)
		fmt.Fprintln(out, line+textfmt.ClipRune(ask, room-1))
	}
	if total > len(rows) {
		fmt.Fprintf(out, "…%d more (-n 0 for all)\n", total-len(rows))
	}
}
