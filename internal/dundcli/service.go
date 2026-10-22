package dundcli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iodesystems/dun/internal/dund"
)

// dund as a user service (CLI-10): systemd on Linux, launchd on macOS, with
// the same verbs. Same verbs and unit shape as `raglit service`, `corrallm
// service` and `aw4 service`.
//
// Without a service, dund runs only while some TUI has started it, and never
// with --http — so dun.iodesystems.com has nothing behind it after a reboot.

// ServiceUnit is the service name a TUI looks for before starting dund itself.
const ServiceUnit = "dund"

// manager is how this OS runs a user service.
type manager interface {
	// path is the service's file: a systemd unit or a launchd plist.
	path(name string) (string, error)
	render(name, execPath string, prefix []string, home string, serveArgs, envs []string) string
	// load makes a freshly written file take effect.
	load(name, path string, enable, start bool) error
	unload(name string) error
	verb(verb, name string) error // start, stop, restart, status
	logs(name, home string) error
}

// serviceManager is this OS's: launchd on macOS, systemd elsewhere.
func serviceManager() manager {
	if runtime.GOOS == "darwin" {
		return launchd{}
	}
	return systemd{}
}

// ServiceFile is the installed service's file, and whether it exists.
func ServiceFile(name string) (string, bool) {
	p, err := serviceManager().path(name)
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return p, false
	}
	return p, true
}

// StartService starts the installed service (the TUI, when dund is down).
func StartService(name string) error { return serviceManager().verb("start", name) }

type systemd struct{}

func (systemd) path(name string) (string, error) { return UnitPath(name) }

func (systemd) render(_, execPath string, prefix []string, home string, serveArgs, envs []string) string {
	return renderUnit(execPath, prefix, home, serveArgs, envs)
}

func (systemd) load(name, _ string, enable, start bool) error {
	if err := systemctl("daemon-reload").Run(); err != nil {
		return err
	}
	if enable {
		if err := systemctl("enable", name).Run(); err != nil {
			return err
		}
	}
	if start {
		return systemctl("restart", name).Run()
	}
	return nil
}

func (systemd) unload(name string) error { return systemctl("disable", "--now", name).Run() }

func (systemd) verb(verb, name string) error {
	if verb == "status" {
		return systemctl(verb, name).Run()
	}
	out, err := exec.Command("systemctl", "--user", verb, name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s %s: %v: %s", verb, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemd) logs(name, _ string) error {
	cmd := exec.Command("journalctl", "--user", "-u", name, "-f", "-n", "80")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// withPATH adds the installer's PATH to envs unless one is given: a service
// does not inherit a login shell's PATH (launchd's has no /opt/homebrew/bin;
// systemd's user manager's is whatever it was started with), and the
// engines run git, mcpshell, poly-lsp-mcp and raglit from it.
func withPATH(envs []string) []string {
	for _, e := range envs {
		if strings.HasPrefix(e, "PATH=") {
			return envs
		}
	}
	return append(envs, "PATH="+os.Getenv("PATH"))
}

// UnitPath is where the user unit lives.
func UnitPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", name+".service"), nil
}

func serviceCmd(l *loader) *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "run dund as a user service (systemd on Linux, launchd on macOS)",
		Long: "Everything after `--` is passed to `dund serve` verbatim, and is checked by\n" +
			"`dund serve --check` before a unit is written — under Restart=on-failure a\n" +
			"bad flag is not one error, it is a crash loop.",
	}
	var name, execPath string
	var enable, start bool
	var envs []string
	install := &cobra.Command{
		Use:   "install [-- <serve flags>...]",
		Short: "write the unit",
		RunE: func(cmd *cobra.Command, args []string) error {
			l.load()
			if execPath == "" {
				p, err := os.Executable()
				if err != nil {
					return fmt.Errorf("locate this binary (pass --exec): %w", err)
				}
				// Symlinks resolved, so the unit does not follow whatever a
				// later install points the name at.
				if r, err := filepath.EvalSymlinks(p); err == nil {
					p = r
				}
				execPath = p
			}
			abs, err := filepath.Abs(execPath)
			if err != nil {
				return err
			}
			execPath = abs
			// The unit runs `<exec> dund serve` when exec is dun itself, and
			// `<exec> serve` when it is the standalone dund.
			prefix := servePrefix(execPath)

			chk := exec.Command(execPath, append(append(append([]string{}, prefix...), "--home", l.home, "serve", "--check"), args...)...)
			chk.Stdout, chk.Stderr = os.Stderr, os.Stderr
			if err := chk.Run(); err != nil {
				return errors.New("`serve --check` failed with those flags — refusing to write a unit that would crash-loop")
			}

			m := serviceManager()
			path, err := m.path(name)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			unit := m.render(name, execPath, prefix, l.home, args, withPATH(envs))
			if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
				return err
			}
			fmt.Printf("wrote %s\n  exec  %s %s\n  home  %s\n  serve %s\n",
				path, execPath, strings.Join(prefix, " "), l.home, strings.Join(args, " "))
			if err := m.load(name, path, enable, start); err != nil {
				return err
			}
			fmt.Println("\n`dun dund service status` to see it, `dun dund service logs` to follow it.")
			return nil
		},
	}
	install.Flags().StringVar(&name, "name", ServiceUnit, "unit name without .service")
	install.Flags().StringVar(&execPath, "exec", "", "binary for the unit (default: this one)")
	install.Flags().BoolVar(&enable, "enable", false, "start at login (macOS: always — a LaunchAgent loads at login)")
	install.Flags().BoolVar(&start, "start", false, "start it now (restarts a running one)")
	install.Flags().StringArrayVar(&envs, "env", nil, "environment as KEY=VALUE (repeatable)")

	uninstall := &cobra.Command{
		Use: "uninstall", Short: "stop it and remove the unit",
		RunE: func(*cobra.Command, []string) error {
			m := serviceManager()
			_ = m.unload(name)
			path, err := m.path(name)
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Printf("removed %s\n", path)
			if _, ok := m.(systemd); ok {
				return systemctl("daemon-reload").Run()
			}
			return nil
		},
	}
	uninstall.Flags().StringVar(&name, "name", ServiceUnit, "unit name without .service")
	c.AddCommand(install, uninstall)
	for _, verb := range []string{"start", "stop", "restart", "status"} {
		v := verb
		sub := &cobra.Command{
			Use: v, Short: v + " the service",
			RunE: func(*cobra.Command, []string) error { return serviceManager().verb(v, name) },
		}
		sub.Flags().StringVar(&name, "name", ServiceUnit, "unit name without .service")
		c.AddCommand(sub)
	}
	logs := &cobra.Command{
		Use: "logs", Short: "follow the service's output",
		RunE: func(*cobra.Command, []string) error {
			l.load()
			return serviceManager().logs(name, l.home)
		},
	}
	logs.Flags().StringVar(&name, "name", ServiceUnit, "unit name without .service")
	c.AddCommand(logs)
	return c
}

// servePrefix is what goes between the binary and "serve".
func servePrefix(execPath string) []string {
	if filepath.Base(execPath) == "dun" {
		return []string{"dund"}
	}
	return nil
}

func systemctl(args ...string) *exec.Cmd {
	c := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c
}

// quoteUnit renders one argument for a systemd Exec line: systemd does its
// own unquoting, so a space would split an argument and a bare % is a
// specifier.
func quoteUnit(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func renderUnit(execPath string, prefix []string, home string, serveArgs, envs []string) string {
	head := []string{quoteUnit(execPath)}
	for _, p := range prefix {
		head = append(head, p)
	}
	head = append(head, "--home", quoteUnit(home))
	var rest []string
	for _, a := range serveArgs {
		rest = append(rest, quoteUnit(a))
	}
	line := func(verb ...string) string {
		return strings.Join(append(append(append([]string{}, head...), verb...), rest...), " ")
	}
	var b strings.Builder
	b.WriteString("# Generated by `dund service install`. Edits are overwritten on reinstall.\n")
	b.WriteString("[Unit]\nDescription=dund — dun's session service\nAfter=network-online.target\nWants=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	// dund resolves nothing relative to the working directory, but systemd
	// would start it in $HOME; its own state directory is the honest answer.
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", quoteUnit(home))
	fmt.Fprintf(&b, "Environment=DUN_HOME=%s\n", quoteUnit(home))
	for _, e := range envs {
		fmt.Fprintf(&b, "Environment=%s\n", quoteUnit(e))
	}
	fmt.Fprintf(&b, "ExecStartPre=%s\n", line("serve", "--check"))
	fmt.Fprintf(&b, "ExecStart=%s\n", line("serve"))
	b.WriteString(`Restart=on-failure
RestartSec=3
# A stop asks every engine to end (10 s grace each, in parallel) before the
# socket closes; a turn in flight is cut short and resumes from the transcript.
TimeoutStopSec=30

[Install]
WantedBy=default.target
`)
	return b.String()
}

// check proves `serve` can run with these settings: the state directory is
// usable, the registry loads, the engine binary is executable, no other dund
// owns this home, and the HTTP address can be bound.
func check(l *loader, bin, httpAddr, webRoot string) error {
	if err := os.MkdirAll(l.home, 0o700); err != nil {
		return fmt.Errorf("home %s: %w", l.home, err)
	}
	svc, err := dund.New(filepath.Join(l.home, "dund"), dund.ExecSpawner{Bin: bin})
	if err != nil {
		return err
	}
	svc.Close()
	st, err := os.Stat(bin)
	if err != nil {
		if p, lerr := exec.LookPath(bin); lerr == nil {
			st, err = os.Stat(p)
		}
	}
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("engine binary %s is not an executable file", bin)
	}
	if pid, alive := pidAlive(l.pidfile()); alive && pid != os.Getpid() {
		return fmt.Errorf("another dund already runs for %s (pid %d) — stop it first", l.home, pid)
	}
	if httpAddr != "" {
		ln, err := net.Listen("tcp", httpAddr)
		if err != nil {
			return fmt.Errorf("cannot bind %s: %w", httpAddr, err)
		}
		ln.Close()
		if !dund.TokenExists(filepath.Join(l.home, "dund")) {
			fmt.Fprintln(os.Stderr, "note: no local token yet — HTTP clients are refused until `dun dund token`")
		}
	}
	if webRoot != "" {
		if st, err := os.Stat(filepath.Join(webRoot, "index.html")); err != nil || st.IsDir() {
			return fmt.Errorf("web root %s has no index.html (pnpm export:web in mobile/)", webRoot)
		}
	}
	fmt.Fprintf(os.Stderr, "dund: check ok (home %s, engines %s", l.home, bin)
	if httpAddr != "" {
		fmt.Fprintf(os.Stderr, ", http %s", httpAddr)
	}
	fmt.Fprintln(os.Stderr, ")")
	return nil
}
