// Package dundcli is the command line of dun's session service, run as
// `dun dund …` (or the standalone dund binary): it runs every session's
// engine, keeps each session's events, and serves clients over a unix socket.
//
//	dund serve       run the service (one per DUN_HOME)
//	dund sessions    list your sessions
//	dund token       make a new local token for HTTP clients (the old one stops working)
//
// Paths: $DUN_HOME (default ~/.dun) holds dund.sock (0600), dund.pid, the
// registry in dund/ and, when a client started the service, dund.log.
package dundcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/proto"
)

// Main runs the command line with args (without the program name) and
// returns the process exit code.
func Main(args []string) int {
	root := newRoot()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "dund:", err)
		return 1
	}
	return 0
}

// Socket is the service's socket for a DUN_HOME: <home>/dund.sock, unless
// that is longer than a unix socket path may be (107 bytes) — then a name
// derived from home in $XDG_RUNTIME_DIR (per user, 0700), else the temp dir.
func Socket(home string) string {
	p := filepath.Join(home, "dund.sock")
	if len(p) <= 107 {
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(dir, "dund-"+hex.EncodeToString(sum[:6])+".sock")
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
	return Socket(l.home)
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
	root.AddCommand(serveCmd(l), sessionsCmd(l), tokenCmd(l), serviceCmd(l), schemaCmd())
	return root
}

func serveCmd(l *loader) *cobra.Command {
	var bin, httpAddr, webRoot, releases string
	var oidcCfg dundapi.OIDC
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			if bin == "" {
				bin = defaultDun()
			}
			if checkOnly {
				return check(l, bin, httpAddr, webRoot)
			}
			if (oidcCfg.Issuer == "") != (oidcCfg.ClientID == "") || (oidcCfg.Issuer != "" && len(oidcCfg.Allow) == 0) {
				return errors.New("sign-in needs --oidc-issuer, --oidc-client-id and at least one --oidc-allow email")
			}
			return serve(cmd.Context(), l, bin, httpAddr, dundapi.Options{WebRoot: webRoot, Releases: releases, OIDC: oidcCfg})
		},
	}
	cmd.Flags().StringVar(&bin, "dun", "", "the dun binary engines run (default: beside dund, else on PATH)")
	cmd.Flags().StringVar(&oidcCfg.Issuer, "oidc-issuer", "", "sign-in: the identity provider (e.g. https://id.iodesystems.com, no trailing slash)")
	cmd.Flags().StringVar(&oidcCfg.ClientID, "oidc-client-id", "", "sign-in: the provider's client id for dund (PKCE, no secret)")
	cmd.Flags().StringArrayVar(&oidcCfg.Allow, "oidc-allow", nil, "sign-in: an email that may sign in (repeatable); each sees the owner's sessions")
	cmd.Flags().StringVar(&oidcCfg.Redirect, "oidc-redirect", "", "sign-in: the callback URL (default: /auth/callback on the address the browser used)")
	cmd.Flags().StringVar(&releases, "releases", "", "serve Android releases from here (tools/release-android): /releases, /dun.apk, announced in /api/v1/hello")
	cmd.Flags().StringVar(&webRoot, "web-root", "", "serve this built client at / (mobile: pnpm export:web); default: the built-in stopgap page")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "prove these settings can run, then exit (the unit's ExecStartPre)")
	cmd.Flags().StringVar(&httpAddr, "http", "", "also serve HTTP here (e.g. 127.0.0.1:20003); needs a token (dund token)")
	return cmd
}

// defaultDun is the binary engines run: this one when it is dun itself (run
// as `dun dund`), else the dun next to it, else the one on PATH.
func defaultDun() string {
	if exe, err := os.Executable(); err == nil {
		if filepath.Base(exe) == "dun" {
			return exe
		}
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

func serve(ctx context.Context, l *loader, bin, httpAddr string, o dundapi.Options) error {
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
	errc := make(chan error, 2)
	go func() { errc <- dund.NewServer(svc, dund.UnixPeer).Serve(ln) }()
	var hs *http.Server
	if httpAddr != "" {
		o.Owner = dundapi.Owner(os.Getuid())
		h, err := dundapi.Handler(svc, o)
		if err != nil {
			ln.Close()
			return err
		}
		hs = &http.Server{Addr: httpAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		hl, err := net.Listen("tcp", httpAddr)
		if err != nil {
			ln.Close()
			return fmt.Errorf("listen %s: %w", httpAddr, err)
		}
		go func() {
			if err := hs.Serve(hl); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		if !svc.HasToken() {
			fmt.Fprintln(os.Stderr, "dund: no local token yet — HTTP clients are refused until `dun dund token`")
		}
		fmt.Fprintf(os.Stderr, "dund: HTTP on %s\n", httpAddr)
	}
	self, _ := os.Executable() // now: after a rebuild it names the deleted file
	rebuilt := watchSelf(ctx, svc, self)
	fmt.Fprintf(os.Stderr, "dund: serving %s (engines: %s)\n", sock, bin)
	reexec := false
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "dund: stopping engines")
	case err = <-errc:
	case <-rebuilt:
		fmt.Fprintln(os.Stderr, "dund: a new build is in place and nothing is busy — restarting into it")
		reexec = true
	}
	if hs != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = hs.Shutdown(sctx)
		cancel()
	}
	ln.Close()
	os.Remove(sock)
	svc.Close()
	if reexec {
		return syscall.Exec(self, os.Args, os.Environ())
	}
	return err
}

// selfCheck is how often dund looks at its own binary.
const selfCheck = 2 * time.Second

// watchSelf fires once this process's executable has been replaced (a
// rebuild renames a new binary over it) AND no session is busy, so that a
// restart cuts no turn, question, job or sub-agent short. Clients reconnect
// on their own; engines resume on first use.
func watchSelf(ctx context.Context, svc *dund.Service, exe string) <-chan struct{} {
	out := make(chan struct{})
	start, err := os.Stat(exe)
	if err != nil {
		return out
	}
	go func() {
		t := time.NewTicker(selfCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			now, err := os.Stat(exe)
			if err != nil || os.SameFile(start, now) && now.ModTime().Equal(start.ModTime()) {
				continue
			}
			if svc.Busy() {
				continue
			}
			close(out)
			return
		}
	}()
	return out
}

// claimPidfile refuses to start while another dund owns this home, and
// replaces a pidfile whose process is gone.
func claimPidfile(path string) error {
	if pid, alive := pidAlive(path); alive && pid != os.Getpid() {
		return fmt.Errorf("already running (pid %d, %s)", pid, path)
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// pidAlive is the pid a pidfile names and whether that process exists.
func pidAlive(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, syscall.Kill(pid, 0) == nil
}

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "print the GraphQL schema (SDL) clients generate their types from",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			sdl, err := dundapi.SDL()
			if err != nil {
				return err
			}
			fmt.Print(sdl)
			return nil
		},
	}
}

func tokenCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "make a new local token for HTTP clients; the previous one stops working",
		Long: "Prints a new bearer token for clients that reach dund over HTTP (a browser on\n" +
			"localhost, the app on the LAN). Only its hash is kept, so this is the one time\n" +
			"it is shown; run it again to replace it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			tok, err := dund.NewToken(filepath.Join(l.home, "dund"))
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
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
