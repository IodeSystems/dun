// Command dund is dun's session service: it runs every session's engine,
// keeps each session's events, and serves clients over a unix socket.
//
//	dund serve       run the service (one per DUN_HOME)
//	dund sessions    list your sessions
//
// Paths: $DUN_HOME (default ~/.dun) holds dund.sock (0600), dund.pid and the
// registry in dund/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/proto"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "dund:", err)
		os.Exit(1)
	}
}

// loader holds the flags every command shares.
type loader struct {
	home string
	sock string
}

func (l *loader) load() {
	if l.home == "" {
		l.home = os.Getenv("DUN_HOME")
	}
	if l.home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			l.home = filepath.Join(h, ".dun")
		}
	}
}

func (l *loader) socket() string {
	if l.sock != "" {
		return l.sock
	}
	return filepath.Join(l.home, "dund.sock")
}
func (l *loader) pidfile() string { return filepath.Join(l.home, "dund.pid") }

func newRoot() *cobra.Command {
	l := &loader{}
	root := &cobra.Command{
		Use:           "dund",
		Short:         "dun's session service",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&l.home, "home", "", "state directory (default $DUN_HOME, else ~/.dun)")
	root.PersistentFlags().StringVar(&l.sock, "socket", "", "unix socket (default <home>/dund.sock)")
	root.AddCommand(serveCmd(l), sessionsCmd(l))
	return root
}

func serveCmd(l *loader) *cobra.Command {
	var bin string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			if bin == "" {
				bin = defaultDun()
			}
			return serve(cmd.Context(), l, bin)
		},
	}
	cmd.Flags().StringVar(&bin, "dun", "", "the dun binary engines run (default: beside dund, else on PATH)")
	return cmd
}

// defaultDun is the dun next to this binary, or the one on PATH.
func defaultDun() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "dun")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("dun"); err == nil {
		return p
	}
	return "dun"
}

func serve(ctx context.Context, l *loader, bin string) error {
	if err := os.MkdirAll(l.home, 0o700); err != nil {
		return err
	}
	if err := claimPidfile(l.pidfile()); err != nil {
		return err
	}
	defer os.Remove(l.pidfile())

	svc, err := dund.New(filepath.Join(l.home, "dund"), dund.ExecSpawner{Bin: bin})
	if err != nil {
		return err
	}
	sock := l.socket()
	// The kernel's limit on a socket path is 108 bytes; past it bind fails
	// with "invalid argument", which names nothing.
	if len(sock) > 107 {
		return fmt.Errorf("socket path is %d bytes, over the 107 a unix socket allows: %s (use --socket)", len(sock), sock)
	}
	_ = os.Remove(sock) // a stale socket; the pidfile says no service owns it
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- dund.NewServer(svc, dund.UnixPeer).Serve(ln) }()
	fmt.Fprintf(os.Stderr, "dund: serving %s (engines: %s)\n", sock, bin)
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "dund: stopping engines")
	case err = <-errc:
	}
	ln.Close()
	os.Remove(sock)
	svc.Close()
	return err
}

// claimPidfile refuses to start while another dund owns this home, and
// replaces a pidfile whose process is gone.
func claimPidfile(path string) error {
	if b, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			if syscall.Kill(pid, 0) == nil {
				return fmt.Errorf("already running (pid %d, %s)", pid, path)
			}
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func sessionsCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "list your sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			c, err := net.DialTimeout("unix", l.socket(), 2*time.Second)
			if err != nil {
				return fmt.Errorf("no service at %s (dund serve): %w", l.socket(), err)
			}
			defer c.Close()
			if err := json.NewEncoder(c).Encode(proto.NewCommand("ls", &proto.ListSessions{})); err != nil {
				return err
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			dec := json.NewDecoder(c)
			for {
				var ev proto.Event
				if err := dec.Decode(&ev); err != nil {
					return err
				}
				if ev.Re != "ls" {
					continue
				}
				if a, ok := ev.Payload.(*proto.Ack); ok && a.Error != "" {
					return errors.New(a.Error)
				}
				list, ok := ev.Payload.(*proto.SessionList)
				if !ok {
					continue
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tRUNNING\tEVENTS\tWORKSPACE")
				for _, s := range list.Items {
					fmt.Fprintf(w, "%s\t%v\t%d\t%s\n", s.ID, s.Running, s.LastSeq, s.Workspace)
				}
				return w.Flush()
			}
		},
	}
}
