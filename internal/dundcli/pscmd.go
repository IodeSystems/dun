package dundcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/procps"
	"github.com/iodesystems/dun/proto"
)

// addPS adds `ps`: under `sessions` (dund sessions ps) and at the top (dund
// ps, and dun ps through cmd/dun).
func addPS(root *cobra.Command, l *loader) {
	for _, c := range root.Commands() {
		if c.Name() == "sessions" {
			c.AddCommand(psCmd(l))
		}
	}
	root.AddCommand(psCmd(l))
}

// sampleWindow is how long CPU and IO are measured over for one table.
const sampleWindow = time.Second

func psCmd(l *loader) *cobra.Command {
	var asJSON bool
	var watch string
	cmd := &cobra.Command{
		Use:   "ps [--json] [--watch [interval]]",
		Short: "every process dun runs, by session: CPU, memory, IO",
		Long: `Every process dun is responsible for on this machine, by session: each
engine and what it runs (its shell, poly-lsp proxy, tool servers, background
jobs), the language servers of the shared poly-lsp-mcp daemon by the session
whose tree is their root, and dun's processes left to pid 1.

Under dund's row: retrieval inside dund (its indexes' ingest and
embedding run in the dund process). The embed backend, then one line per
active index, "rag <project> <kind>  state  queue N  files/min  embeds/min
err", by the session whose workspace is that project; indexes idle for
10 minutes fold into one "N idle indexes" line. --json has every index
with its counters.

CPU% and IO are measured over one second, not averaged over the process's
life. SESSION (none) is a process no live session owns: an orphan, or a
language server for a root no session works in.

dun ps --watch on a terminal is interactive: enter folds a session or shows
a process in full, S cycles the sort (CPU, MEM, IO, AGE), k kills, s stops
the session, o opens it in dun, r restarts the poly-lsp daemon, q quits.
Piped, it redraws the table; --json prints one reading per interval.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l.load()
			if len(args) == 1 {
				if watch == "" {
					return fmt.Errorf("unexpected argument %q (an interval goes with --watch)", args[0])
				}
				watch = args[0]
			}
			out := cmd.OutOrStdout()
			if watch == "" {
				return psOnce(l, out, asJSON)
			}
			every, err := parseInterval(watch)
			if err != nil {
				return err
			}
			return psWatch(cmd.Context(), l, out, asJSON, every)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "the rows as JSON, grouped by session")
	cmd.Flags().StringVar(&watch, "watch", "", "redraw every interval, like top (default 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "2s"
	return cmd
}

// parseInterval reads 2s, 500ms or a bare number of seconds.
func parseInterval(s string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		s = fmt.Sprintf("%gs", n)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 200*time.Millisecond {
		return 0, fmt.Errorf("--watch %q: give an interval of at least 200ms, as 2s or 2", s)
	}
	return d, nil
}

// PSView is one table's worth: what it shows and what it says about it.
type PSView struct {
	At       time.Time      `json:"at"`
	Window   float64        `json:"window_sec"`
	Groups   []procps.Group `json:"groups"`
	Sessions int            `json:"sessions"`
	Note     string         `json:"note,omitempty"`
	// List is dund's answer (nil: dund did not answer); the interactive
	// view reads each session's workspace from it.
	List *proto.ProcsList `json:"-"`
}

// PSSource is what the interactive view (Interactive) reads and acts with.
type PSSource struct {
	// Sample is the next reading, rated against the one before it (the
	// first waits sampleWindow), with the table it was read from.
	Sample func() (PSView, procps.Snapshot, error)
	// StopSession stops a session's engine through dund.
	StopSession func(id string) error
	// Every is the refresh interval.
	Every time.Duration
}

// Interactive is `ps --watch` on a terminal: the bubbletea view in
// internal/psview. cmd/dun sets it; dundcli cannot import it, because
// internal/tui (whose styles it uses) imports dundcli. Nil (dund's own
// binary): the redrawn table.
var Interactive func(ctx context.Context, src PSSource) error

// sample reads dund's sessions and the table, rated against prev (or
// against a reading sampleWindow before, when prev is empty).
func sample(l *loader, prev procps.Snapshot, rc *rootsCache) (PSView, procps.Snapshot, error) {
	if prev.Procs == nil {
		s, err := procps.Read()
		if err != nil {
			return PSView{}, prev, err
		}
		prev = s
		time.Sleep(sampleWindow)
	}
	list, lerr := fetchProcs(l)
	cur, err := procps.Read()
	if err != nil {
		return PSView{}, prev, err
	}
	rows := procps.Attribute(cur, list, procps.Cwd)
	procps.Apply(rows, procps.Rates(prev, cur))
	if d := procps.PolyDaemon(cur); d != nil {
		roots, rerr := rc.get(d, cur.At)
		procps.AttachRoots(rows, d.PID, roots, rerr, list)
	}
	var sessions []proto.ProcSession
	if list != nil {
		sessions = list.Sessions
		procps.AttachRag(rows, list.DundPID, list.Rag, sessions, cur.At)
	}
	// The raglit daemon's indexes: `raglit status --json --all` with dun's
	// login token, at most every rootsEvery.
	if d := procps.RaglitDaemon(cur); d != nil {
		r, rerr := rc.getRaglit(d, cur.At)
		procps.AttachRaglit(rows, d.PID, r, rerr, sessions, cur.At)
	}
	v := PSView{At: cur.At, Window: cur.At.Sub(prev.At).Seconds(), Groups: procps.Groups(rows), List: list}
	if list != nil {
		v.Sessions = len(list.Sessions)
	}
	var notes []string
	if lerr != nil {
		notes = append(notes, "sessions unknown, only the shared daemons and orphans are shown: "+lerr.Error())
	}
	if procps.IONote != "" {
		notes = append(notes, procps.IONote)
	}
	v.Note = strings.Join(notes, "; ")
	return v, cur, nil
}

// rootsEvery is the least time between two `poly-lsp-mcp status` runs in
// --watch: it walks the daemon's status files.
const rootsEvery = 10 * time.Second

// rootsCache holds the last reading of the daemon's roots (or its failure).
type rootsCache struct {
	at    time.Time
	pid   int
	roots []procps.Root
	err   error
	read  func(*procps.Proc) ([]procps.Root, error) // procps.ReadRoots; tests replace it

	ragAt   time.Time
	ragPID  int
	rag     *procps.RaglitReading
	ragErr  error
	readRag func(*procps.Proc, time.Time) (*procps.RaglitReading, error) // tests replace it
}

// getRaglit is the raglit daemon's last status, read at most every rootsEvery.
func (c *rootsCache) getRaglit(d *procps.Proc, now time.Time) (*procps.RaglitReading, error) {
	if c.ragPID != d.PID || c.ragAt.IsZero() || now.Sub(c.ragAt) >= rootsEvery {
		read := c.readRag
		if read == nil {
			read = func(d *procps.Proc, now time.Time) (*procps.RaglitReading, error) {
				return procps.ReadRaglit(d, harness.RaglitTokenEnv(), now)
			}
		}
		c.rag, c.ragErr = read(d, now)
		c.ragAt, c.ragPID = now, d.PID
	}
	return c.rag, c.ragErr
}

func (c *rootsCache) get(d *procps.Proc, now time.Time) ([]procps.Root, error) {
	if c.pid != d.PID || c.at.IsZero() || now.Sub(c.at) >= rootsEvery {
		read := c.read
		if read == nil {
			read = procps.ReadRoots
		}
		c.roots, c.err = read(d)
		c.at, c.pid = now, d.PID
	}
	return c.roots, c.err
}

func psOnce(l *loader, w io.Writer, asJSON bool) error {
	v, _, err := sample(l, procps.Snapshot{}, &rootsCache{})
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	width, _ := termSize(w)
	_, err = io.WriteString(w, render(v, width, 0))
	return err
}

func psWatch(ctx context.Context, l *loader, w io.Writer, asJSON bool, every time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var prev procps.Snapshot
	rc := &rootsCache{}
	if !asJSON && Interactive != nil && isTTY(w) && isTTY(os.Stdin) {
		return Interactive(ctx, PSSource{
			Sample: func() (PSView, procps.Snapshot, error) {
				v, cur, err := sample(l, prev, rc)
				if err == nil {
					prev = cur
				}
				return v, cur, err
			},
			StopSession: func(id string) error { return stopSession(l, id) },
			Every:       every,
		})
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	for {
		v, cur, err := sample(l, prev, rc)
		if err != nil {
			return err
		}
		prev = cur
		if asJSON {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				return err
			}
		} else {
			width, height := termSize(w)
			// Home, clear, the table: a redraw in place, as top does.
			fmt.Fprint(w, "\x1b[H\x1b[2J"+render(v, width, height))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// render is the table and its footer, cut to height lines (0: all).
func render(v PSView, width, height int) string {
	body := procps.Table(v.Groups, width)
	n := 0
	for _, g := range v.Groups {
		n += len(g.Rows)
	}
	foot := fmt.Sprintf("%s, %s running; CPU%% and IO over %.1fs; %s",
		plural(n, "process", "processes"), plural(v.Sessions, "session", "sessions"), v.Window, v.At.Format("15:04:05"))
	if v.Note != "" {
		foot += "\n" + v.Note
	}
	if height > 0 {
		lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
		room := height - 1 - strings.Count(foot, "\n") - 1
		if room > 0 && len(lines) > room {
			lines = append(lines[:room-1], fmt.Sprintf("… %d more lines", len(lines)-room+1))
		}
		body = strings.Join(lines, "\n") + "\n"
	}
	return body + "\n" + foot + "\n"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// termSize is w's terminal size, else $COLUMNS wide; 0, 0 when w is not a
// terminal (piped output is not cut).
func termSize(w io.Writer) (int, int) {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0, 0
	}
	if cw, ch, err := term.GetSize(int(f.Fd())); err == nil && cw > 0 {
		return cw, ch
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil {
		return c, 0
	}
	return 80, 0
}

func isTTY(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// stopSession asks dund to stop session id's engine (proto.Stop), and
// waits for its answer.
func stopSession(l *loader, id string) error {
	c, err := net.DialTimeout("unix", l.socket(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("no service at %s", l.socket())
	}
	defer c.Close()
	cmd := proto.NewCommand("stop", &proto.Stop{})
	cmd.Session = id
	if err := json.NewEncoder(c).Encode(cmd); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(c)
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		if a, ok := ev.Payload.(*proto.Ack); ok && ev.Re == "stop" {
			if a.Error != "" {
				return errors.New(a.Error)
			}
			return nil
		}
	}
}

// fetchProcs asks dund for its running sessions.
func fetchProcs(l *loader) (*proto.ProcsList, error) {
	c, err := net.DialTimeout("unix", l.socket(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no service at %s", l.socket())
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(proto.NewCommand("ps", &proto.Procs{})); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(c)
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			return nil, err
		}
		if ev.Re != "ps" {
			continue
		}
		switch p := ev.Payload.(type) {
		case *proto.Ack:
			if p.Error != "" {
				// A dund from before ps takes procs for a session's command.
				return nil, errors.New("dund refused it (" + p.Error + "); a dund older than this dun does not answer ps: dun dund service restart")
			}
		case *proto.ProcsList:
			return p, nil
		}
	}
}
