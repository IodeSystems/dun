package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iodesystems/dun/internal/harness"
)

// `dun trace <id>`: where a request's trace id (internal/harness/trace.go)
// was logged on this machine. The id itself names the session and the role;
// the lines found give the round's stats, the turn error and the log line
// with the provider's own id.

const traceHelp = `dun trace — find a request by its trace id

  dun trace <id>

Every model request dun sends carries a trace id in its X-Trace-Id header:
dun-<session>-<run>-<role>-<seq> from a session, dund-<facet>-<role>-<seq>
from dund. Take one from a provider's or gateway's log and this prints the
session and role it names, then each line on this machine that has it: the
round stats ($DUN_HOME/stats), the turn errors (<session>.errors.jsonl) and
the engine and dund logs ($DUN_HOME/dund/engines/*.log, $DUN_HOME/dund.log).
A dund under systemd logs to the journal: journalctl --user -u dund | grep <id>.
`

func traceCmd(args []string, out, errw io.Writer) int {
	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if len(args) == 1 {
			fmt.Fprint(out, traceHelp)
			return 0
		}
		fmt.Fprint(errw, traceHelp)
		return 2
	}
	id := args[0]
	if p, ok := harness.ParseTraceID(id); ok {
		fmt.Fprintf(out, "session %s · run %s · role %s · request %s\n", p.Session, p.Run, p.Role, p.Seq)
	}
	hits := 0
	for _, f := range traceFiles() {
		hits += grepFile(f, id, out)
	}
	if hits == 0 {
		fmt.Fprintf(out, "no line on this machine has %s (looked under %s)\n", id, harness.DunHome())
		return 1
	}
	return 0
}

// traceFiles are the files a trace id is written to.
func traceFiles() []string {
	home := harness.DunHome()
	var out []string
	for _, pat := range []string{
		filepath.Join(home, "stats", "rounds-*.jsonl"),
		filepath.Join(harness.SessionsDir(), "*", "*.errors.jsonl"),
		filepath.Join(home, "dund", "engines", "*.log"),
		filepath.Join(home, "dund.log"),
	} {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	return out
}

// grepFile prints each line of f holding id as file:line: text, and
// returns how many.
func grepFile(f, id string, out io.Writer) int {
	fh, err := os.Open(f)
	if err != nil {
		return 0
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	n := 0
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if !strings.Contains(text, id) {
			continue
		}
		if len(text) > 600 {
			text = text[:600] + "…"
		}
		fmt.Fprintln(out, f+":"+strconv.Itoa(line)+": "+text)
		n++
	}
	return n
}
