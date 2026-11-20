// Package dundcli is the command line of dun's session service, run as
// `dun dund …` (or the standalone dund binary): it runs every session's
// engine, keeps each session's events, and serves clients over a unix socket.
//
//	dund serve       run the service (one per DUN_HOME)
//	dund sessions    list your sessions
//	dund token       make a new local token for HTTP clients (the old one stops working)
//	dund token create|list|revoke   named API tokens, one per client
//	dund rag         search and fill the retrieval indexes (docs/rag.md)
//	dund enable|disable web|tui   the HTTP listener and /tui, at runtime (web.go)
//
// Paths: $DUN_HOME (default ~/.dun) holds dund.sock (0600), dund.pid, the
// registry in dund/, the retrieval indexes in dund/rag/ and, when a client started the service, dund.log.
package dundcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// Main runs the command line with args (without the program name) and
// returns the process exit code.
func Main(args []string) int {
	root := newRoot()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "dund:", err)
		if errors.Is(err, errHandover) {
			return handoverExit
		}
		return 1
	}
	return 0
}

// maxSocketPath is the longest unix socket path bind accepts: sun_path less
// its NUL — 108 bytes on Linux, 104 on macOS and the BSDs.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// Socket is the service's socket for a DUN_HOME: <home>/dund.sock, unless
// that is longer than a unix socket path may be (maxSocketPath) — then a
// name derived from home in $XDG_RUNTIME_DIR (per user, 0700), else the temp
// dir.
func Socket(home string) string { return SocketFile(home, "dund") }

// SocketFile is Socket for any of DUN_HOME's sockets: <home>/<name>.sock, or
// <name>-<hash of home>.sock in the runtime dir when that is too long.
func SocketFile(home, name string) string {
	p := filepath.Join(home, name+".sock")
	if len(p) <= maxSocketPath() {
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(dir, name+"-"+hex.EncodeToString(sum[:6])+".sock")
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
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&l.home, "home", "", "state directory (default $DUN_HOME, else ~/.dun)")
	root.PersistentFlags().StringVar(&l.sock, "socket", "", "unix socket (default <home>/dund.sock)")
	root.AddCommand(serveCmd(l), sessionsCmd(l), tokenCmd(l), serviceCmd(l), schemaCmd(), ragCmd(l), enableCmd(l), disableCmd(l))
	addPS(root, l)
	return root
}

func serveCmd(l *loader) *cobra.Command {
	var bin, httpAddr, webRoot, releases string
	var oidcCfg dundapi.OIDC
	var checkOnly bool
	var handoverFrom int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "run the service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			if bin == "" {
				bin = defaultDun()
			}
			webRoot = resolveWebRoot(webRoot, dundapi.HasEmbeddedApp())
			if checkOnly {
				return check(l, bin, httpAddr, webRoot, handoverFrom)
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
	cmd.Flags().StringVar(&webRoot, "web-root", "", "serve this built web client at /web (mobile: pnpm export:web); default: the copy a release build embeds, else $XDG_DATA_HOME/dun/web (~/.local/share/dun/web, where `make web` installs it) when it has index.html")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "prove these settings can run, then exit (the unit's ExecStartPre)")
	// The web app and its HTTP API are not tested yet (USER 2026-10-28: "it's
	// not ready, it's not tested"), so the flag says so. --http was its name:
	// still accepted, with a warning, so an installed service keeps running.
	// Since 2026-11-17 the flag only seeds <home>/dund/web.json (web.go);
	// `dun dund enable web` is the way to turn it on.
	cmd.Flags().StringVar(&httpAddr, "web-experimental", "", "EXPERIMENTAL: seed the HTTP listener (web app, TUI, API) at this address while <home>/dund/web.json does not exist; use `dund enable web` instead")
	cmd.Flags().StringVar(&httpAddr, "http", "", "")
	_ = cmd.Flags().MarkDeprecated("http", "use --web-experimental (the web app is not tested yet)")
	// The check a service's dund runs on a new build before it hands over
	// (serveCheck): that live dund owns the pidfile and the HTTP port.
	cmd.Flags().IntVar(&handoverFrom, "handover-from", 0, "with --check: the live dund (pid) this build replaces")
	_ = cmd.Flags().MarkHidden("handover-from")
	return cmd
}

// engineSpawner starts engines on the engine link in <home>/dund/engines,
// so they outlive dund's restart (docs/engine-adoption.md): in a systemd
// scope of their own when systemd runs dund, which would otherwise end
// them with its cgroup. Version is bin's build, which an adopted engine of
// another build moves to at its next idle.
func engineSpawner(home, bin string) dund.ExecSpawner {
	sp := dund.ExecSpawner{Bin: bin, EnginesDir: filepath.Join(home, "dund", "engines"),
		Scope: runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != ""}
	if self, err := os.Executable(); err == nil && sameFile(self, bin) {
		sp.Version = Version
	} else {
		sp.Version = ProbeVersion(bin)
	}
	return sp
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
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

	svc, err := dund.New(filepath.Join(l.home, "dund"), engineSpawner(l.home, bin))
	if err != nil {
		return err
	}
	sock := l.socket()
	// Past the kernel's limit bind fails with "invalid argument", which
	// names nothing.
	if n := maxSocketPath(); len(sock) > n {
		return fmt.Errorf("socket path is %d bytes, over the %d a unix socket allows here: %s (use --socket)", len(sock), n, sock)
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
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)
	errc := make(chan error, 2)
	self, _ := os.Executable() // now: after a rebuild it names the deleted file
	dundapi.Version = Version
	srv := dund.NewServer(svc, dund.UnixPeer)
	serveArgs := slices.Clone(os.Args[1:]) // the service's flags, for the new build's check
	var upgrade func(exe, version string) error
	handover := false
	switch {
	case !underServiceManager():
		upgrade = func(exe, version string) error { return requestUpgrade(svc, exe, version) }
	default:
		// A service: the upgrade installs the new build where the unit
		// starts it, and exits for the manager to restart it (handover.go).
		if why := canHandOver(self); why != nil {
			srv.SetNoUpgrade(why.Error())
			break
		}
		handover = true
		upgrade = func(exe, version string) error {
			ver, err := handoverCandidate(self, exe, version)
			if err == nil {
				err = serveCheck(exe, serveArgs) // said now, not at the quiet point
			}
			if err != nil {
				return err
			}
			return svc.RequestUpgrade(exe, ver)
		}
		svc.SetUpgradeInstall(func(exe, version string) error {
			return installHandover(self, exe, version, serveArgs)
		})
	}
	srv.SetBuild(Version, self, upgrade)
	rag, err := openRag(l.home)
	if err != nil {
		ln.Close()
		return err
	}
	srv.SetRag(rag)
	// The HTTP listener (web.go): web.json decides; the flag seeds it.
	webSt, seeded, err := effectiveWeb(l.home, httpAddr)
	if err != nil {
		ln.Close()
		rag.Close(0)
		return err
	}
	web, err := newWebCtl(l.home, bin, svc, o)
	if err != nil {
		ln.Close()
		rag.Close(0)
		return err
	}
	if seeded {
		if err := saveWebState(l.home, webSt); err != nil {
			fmt.Fprintf(os.Stderr, "dund: web: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "dund: web: saved the flag's %s in %s; from now on it decides (dun dund enable|disable web)\n", httpAddr, webStatePath(l.home))
		}
	} else if httpAddr != "" {
		fmt.Fprintf(os.Stderr, "dund: web: %s decides; the flag's %s is not used (dun dund enable web --addr to change it)\n", webStatePath(l.home), httpAddr)
	}
	web.start(webSt)
	if webSt.Enabled && !svc.HasToken() {
		fmt.Fprintln(os.Stderr, "dund: no local token yet — HTTP clients are refused until `dun dund token`")
	}
	srv.SetWeb(web.set)
	go func() { errc <- srv.Serve(ln) }()
	go watchSelf(ctx, svc, self)
	fmt.Fprintf(os.Stderr, "dund: serving %s (engines: %s)\n", sock, bin)
	reexec, detached := false, false
	select {
	case sig := <-sigc:
		// A planned stop (a service restart, a kill): sessions that are
		// working are interrupted, recorded, and resumed by the next start.
		fmt.Fprintf(os.Stderr, "dund: %v — stopping engines\n", sig)
		if perr := svc.PrepareStop(fmt.Sprintf("dund was stopped (%v)", sig)); perr != nil {
			// A signal is not refused: the engines stop without a record.
			fmt.Fprintf(os.Stderr, "dund: %v\n", perr)
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "dund: stopping engines")
	case err = <-errc:
	case exe := <-svc.UpgradeReady():
		// The upgrade's waiting is over (or was cut short and recorded):
		// restart into the new binary, engines too, so the two agree.
		// The engines on the link run on; the next dund adopts them
		// (docs/engine-adoption.md). Before the drain, so clients get
		// every event the engines' last words put in the logs.
		svc.Detach()
		detached = true
		if handover {
			// The new build is already at self: the manager starts it.
			fmt.Fprintf(os.Stderr, "dund: %s is the new build — exiting %d for the service manager to start it\n", self, handoverExit)
			err = errHandover
			srv.Drain(time.Second)
			break
		}
		fmt.Fprintf(os.Stderr, "dund: restarting into %s\n", exe)
		if exe != self {
			os.Args = withFlag(os.Args, "--dun", exe)
		}
		self, reexec = exe, true
		srv.Drain(time.Second) // clients read "restarting" before the socket goes
	}
	// Every path out: the TUIs in browser tabs are killed (their sockets are
	// hijacked, which Shutdown leaves open) and the listener stops, before a
	// re-exec or a handover's exit.
	web.stop()
	ln.Close()
	os.Remove(sock)
	// Before the re-exec too: a job cut off here goes back on the queue for
	// the next dund, rather than showing as an error.
	rag.Close(ragd.DrainGrace)
	if !detached {
		svc.Close() // a stop: every engine ends (PrepareStop recorded the work)
	}
	if reexec {
		return syscall.Exec(self, os.Args, os.Environ())
	}
	return err
}

// openRag starts retrieval (docs/rag.md): the indexes under
// <home>/dund/rag, with the model roles from <home>/config.json's facets,
// each said once.
func openRag(home string) (*ragd.Host, error) {
	roles := ragd.Resolve(dunconfig.LoadConfigFile(filepath.Join(home, "config.json")))
	roles.CapsDir = filepath.Join(home, "capabilities")
	for _, line := range roles.Said {
		fmt.Fprintln(os.Stderr, "dund: rag:", line)
	}
	return ragd.Open(filepath.Join(home, "dund", "rag"), roles, nil)
}

// Version is the build this dund runs, set by the binary that embeds it.
var Version = "dev"

// underServiceManager reports whether systemd or launchd started this
// process: then an upgrade puts the new build where the unit starts it and
// exits for the manager to restart it (handover.go), never a re-exec into a
// client's binary.
func underServiceManager() bool {
	return os.Getenv("INVOCATION_ID") != "" || strings.HasPrefix(os.Getenv("XPC_SERVICE_NAME"), "com.iodesystems.")
}

// requestUpgrade checks exe and makes the upgrade into it pending (see
// dund.Service.RequestUpgrade): it answers at once; the restart follows
// when nothing is busy, or on "now", or after the grace period.
func requestUpgrade(svc *dund.Service, exe, version string) error {
	if err := checkUpgradeExe(exe); err != nil {
		return err
	}
	if version == "" {
		version = ProbeVersion(exe)
	}
	return svc.RequestUpgrade(exe, version)
}

// checkUpgradeExe: an upgrade names an absolute, executable, regular file of
// this user.
func checkUpgradeExe(exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("upgrade: %q is not an absolute path", exe)
	}
	st, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("upgrade: %s is not an executable file", exe)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("upgrade: %s is not owned by this user", exe)
	}
	return nil
}

// ProbeVersion asks a dun or dund binary which build it is (`--version`,
// whose last word is the version); "" when it does not say in time.
func ProbeVersion(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	// A child: no self-rebuild, no launcher.
	cmd.Env = append(os.Environ(), "DUN_CHILD=1", "DUN_NO_AUTOBUILD=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return lastWord(string(out))
}

// lastWord is the first line's last word: "dun v1.2" and "dund version v1.2"
// both name v1.2.
func lastWord(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// withFlag sets --name's value in args (either spelling), adding it if absent.
func withFlag(args []string, name, value string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == name && i+1 < len(out):
			out[i+1] = value
			return out
		case strings.HasPrefix(out[i], name+"="):
			out[i] = name + "=" + value
			return out
		}
	}
	return append(out, name, value)
}

// selfCheck is how often dund looks at its own binary.
const selfCheck = 2 * time.Second

// watchSelf makes an upgrade pending each time this process's executable is
// replaced (a rebuild renames a new binary over it), until ctx ends. The
// upgrade restarts once nothing is busy (see dund.Service.RequestUpgrade);
// clients reconnect on their own, engines resume on first use. For a service
// exe is the unit's binary, so a new build put there (the launcher's rebuild,
// a copy) hands over the same way; `make install` writes ~/.local/bin/dun
// instead, and reaches the service through the next TUI of that build, which
// asks it to upgrade.
func watchSelf(ctx context.Context, svc *dund.Service, exe string) {
	last, err := os.Stat(exe)
	if err != nil {
		return
	}
	t := time.NewTicker(selfCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := os.Stat(exe)
		if err != nil || os.SameFile(last, now) && now.ModTime().Equal(last.ModTime()) {
			continue
		}
		last = now
		ver := ProbeVersion(exe)
		fmt.Fprintf(os.Stderr, "dund: a new build (%s) is in place — restarting into it once nothing is busy\n", orNew(ver))
		if err := svc.RequestUpgrade(exe, ver); err != nil {
			fmt.Fprintf(os.Stderr, "dund: %v\n", err)
		}
	}
}

func orNew(v string) string {
	if v == "" {
		return "version unknown"
	}
	return v
}

// claimPidfile refuses to start while another dund owns this home, and
// replaces a pidfile whose process is gone.
func claimPidfile(path string) error {
	if pid, alive := pidAlive(path); alive && pid != os.Getpid() {
		return fmt.Errorf("already running (pid %d, %s)", pid, path)
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// Running is the pid of the dund that owns home, and whether it is alive —
// which, with nothing answering its socket, means it is still stopping.
func Running(home string) (int, bool) { return pidAlive(filepath.Join(home, "dund.pid")) }

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
	cmd := &cobra.Command{
		Use:   "token",
		Short: "make a new local token for HTTP clients; the previous one stops working",
		Long: "Prints a new bearer token for clients that reach dund over HTTP (a browser on\n" +
			"localhost, the app on the LAN). Only its hash is kept, so this is the one time\n" +
			"it is shown; run it again to replace it. `token create|list|revoke` manage\n" +
			"named tokens instead, each revoked on its own.",
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
	cmd.AddCommand(tokenCreateCmd(l), tokenListCmd(l), tokenRevokeCmd(l))
	return cmd
}

func tokenCreateCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "make a named API token; shown once",
		Long: "Prints a new bearer token called <name>, which acts as you over HTTP. Only its\n" +
			"hash is kept. A name in use is refused; revoke it first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l.load()
			tok, err := dund.CreateAPIToken(filepath.Join(l.home, "dund"), args[0])
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
}

func tokenListCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list named API tokens (name, created; never the secret)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l.load()
			toks, err := dund.ListAPITokens(filepath.Join(l.home, "dund"))
			if err != nil {
				return err
			}
			for _, t := range toks {
				fmt.Printf("%s\t%s\n", t.Name, t.Created.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func tokenRevokeCmd(l *loader) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "revoke a named API token; takes effect at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l.load()
			return dund.RevokeAPIToken(filepath.Join(l.home, "dund"), args[0])
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
				fmt.Fprintln(w, "ID\tRUNNING\tATTACHED\tEVENTS\tWORKSPACE")
				for _, s := range list.Items {
					fmt.Fprintf(w, "%s\t%v\t%d\t%d\t%s\n", s.ID, s.Running, s.Attached, s.LastSeq, s.Workspace)
				}
				return w.Flush()
			}
		},
	}
}
