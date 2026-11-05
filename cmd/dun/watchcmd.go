package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/proto"
)

// `dun watch`: one line each time a session's turn ends or it asks for input
// (USER 2026-11-05: "a monitor tool for claude to detect when dun agent is
// done or waiting for input"). An MCP tool cannot do this: a client reads a
// tool's result only when it calls the tool, so a server has no way to tell it
// something later. A command can — Claude Code's Monitor turns each line a
// command prints into a notification, and a background command's exit is one.
// So dun mcp's results name this command, and the model runs it.

const watchHelp = `dun watch — a line each time a dun session ends a turn or asks

  dun watch <session> [--after N] [--once]

Prints one line per turn end:

  <session> · done · event N · <the reply's first line>
  <session> · asking · event N · <question> [option | option]
  <session> · error · event N · <the error>
  <session> · interrupted · event N
  <session> · exited · event N

and exits when the session's engine exits, or after the first line with
--once. It reads the session's events after N (default: its last event, so
only what happens from now on).

For Claude Code: run it with the Monitor tool to be told of each line, or
with --once as a background command to be told once.

Flags:

  --after N      read the events after event N
  --once         exit after the first line
  -h, --help     this text
`

func watchCmd(args []string, out, errw io.Writer) int {
	return watchWith(args, out, errw, tui.DialDund)
}

func watchWith(args []string, out, errw io.Writer, dial func() (net.Conn, error)) int {
	var session string
	var after uint64
	hasAfter, once := false, false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprint(out, watchHelp)
			return 0
		case a == "--once":
			once = true
		case a == "--after" || strings.HasPrefix(a, "--after="):
			v, ok := strings.CutPrefix(a, "--after=")
			if !ok {
				if i+1 >= len(args) {
					fmt.Fprintf(errw, "dun watch: --after needs an event number\n\n%s", watchHelp)
					return 2
				}
				i++
				v = args[i]
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				fmt.Fprintf(errw, "dun watch: --after %q is not an event number\n", v)
				return 2
			}
			after, hasAfter = n, true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "dun watch: no flag %q\n\n%s", a, watchHelp)
			return 2
		case session == "":
			session = a
		default:
			fmt.Fprintf(errw, "dun watch: one session at a time (got %q and %q)\n", session, a)
			return 2
		}
	}
	if session == "" {
		fmt.Fprintf(errw, "dun watch: which session? (dun_sessions, or the session list, names them)\n\n%s", watchHelp)
		return 2
	}
	s := &mcpServer{dial: dial}
	if err := s.watch(session, after, hasAfter, once, out); err != nil {
		// On stdout too: a Monitor reads only stdout, and a watch that died
		// silently looks exactly like a session still working.
		fmt.Fprintf(out, "%s · watch failed · %v\n", session, err)
		return 1
	}
	return 0
}

// watch follows the session's events after seq (its last event, unless
// hasAfter) and prints a line at each turn end, until the engine exits, the
// connection drops, or — with once — the first line.
func (s *mcpServer) watch(session string, after uint64, hasAfter, once bool, out io.Writer) error {
	c, err := s.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if !hasAfter {
		items, err := s.list(c)
		if err != nil {
			return err
		}
		found := false
		for _, it := range items {
			if it.ID == session {
				after, found = it.LastSeq, true
			}
		}
		if !found {
			return fmt.Errorf("no session %s", session)
		}
	}
	sub := proto.NewCommand("sub", &proto.Subscribe{After: after})
	sub.Session = session
	if _, err := c.roundTrip(sub); err != nil {
		return err
	}
	r := mcpReply{session: session, seq: after, mine: true}
	for {
		var ev proto.Event
		if err := c.dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("dund closed the connection")
			}
			return err
		}
		if ev.Session != session {
			continue
		}
		if ev.Seq > r.seq {
			r.seq = ev.Seq
		}
		end := r.take(ev, "")
		if end == "" {
			continue
		}
		fmt.Fprintln(out, r.line(end))
		if once || end == "exited" {
			return nil
		}
		r = mcpReply{session: session, seq: r.seq, mine: true}
	}
}

// line is a turn's end as one line, for dun watch.
func (r mcpReply) line(end string) string {
	s := fmt.Sprintf("%s · %s · event %d", r.session, end, r.seq)
	detail := ""
	switch end {
	case "done":
		if len(r.reply) > 0 {
			detail = firstLine(r.reply[len(r.reply)-1])
		}
	case "asking":
		detail = oneLineText(r.ask.Question)
		if len(r.ask.Options) > 0 {
			detail += " [" + strings.Join(r.ask.Options, " | ") + "]"
		}
	case "error":
		detail = oneLineText(r.err)
	}
	if detail != "" {
		s += " · " + detail
	}
	return s
}

// watchLineCap keeps a line a notification, not a transcript: the reply
// itself is dun_wait's to return.
const watchLineCap = 200

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return oneLineText(l)
		}
	}
	return ""
}

func oneLineText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > watchLineCap {
		return string(r[:watchLineCap]) + "…"
	}
	return s
}
