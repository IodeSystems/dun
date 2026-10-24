package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
	"github.com/spf13/cobra"
)

// `dun doctor` checks what dun needs, read-only, and says how to fix what is
// wrong: the providers (logged in, a model, each one's model list with the
// real reason it fails), the tools the engines run (on this PATH and on the
// service's, which is where launchd differs from a login shell), dund (its
// socket answers this user — the peer-credential check), the service, and a
// self-updating build's source tree and Go. Exit 1 when anything fails.

type finding struct {
	level, area, detail, fix string // level: ok, warn, fail
}

type doctor struct{ found []finding }

func (d *doctor) add(level, area, detail, fix string) {
	d.found = append(d.found, finding{level, area, detail, fix})
}

// doctorCmd is `dun doctor`: the checks, and with --fix the fixes that are
// one command dun can run itself (fixable). The rest are said, as before.
func doctorCmd() *cobra.Command {
	var fix bool
	c := &cobra.Command{
		Use:   "dun doctor",
		Short: "check providers, tools, dund and the service; say how to fix what is wrong",
		Long: "Checks what dun needs, read-only: the providers and their models, the tools the\n" +
			"engines run (on your PATH and the service's), dund and its build, the service,\n" +
			"and a self-updating build's source and Go. Exits 1 when a check fails.\n\n" +
			"--fix runs the fixes that are one command, saying each before it runs, then\n" +
			"checks again:\n  " + strings.Join(fixableNames, "\n  ") + "\n" +
			"The others need you: a login, a model pick, an install.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			if code := runDoctor(fix); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&fix, "fix", false, "run the fixes dun can run itself, then check again")
	c.Flags().BoolP("help", "h", false, "help for dun doctor")
	return c
}

// fixableNames are the fixes --fix runs, for --help.
var fixableNames = []string{
	"a config file others can read: chmod 600",
	"a service on another build: dun dund service restart",
}

// fixable is the action that applies f's fix, or nil when the fix needs the
// person. Matched on the fix's words, which say the same command.
func fixable(f finding) func() error {
	switch {
	case f.level == "ok":
		return nil
	case strings.HasPrefix(f.fix, "chmod 600 "):
		p := strings.TrimPrefix(f.fix, "chmod 600 ")
		return func() error { return os.Chmod(p, 0o600) }
	case strings.HasPrefix(f.fix, "dun dund service restart"):
		return func() error {
			cmd := exec.Command(os.Args[0], "dund", "service", "restart")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		}
	}
	return nil
}

func runDoctor(fix bool) int {
	d := check()
	code := d.report()
	if !fix {
		return code
	}
	ran := 0
	for _, f := range d.found {
		act := fixable(f)
		if act == nil {
			continue
		}
		ran++
		fmt.Printf("\nfixing %s: %s\n", f.area, f.fix)
		if err := act(); err != nil {
			fmt.Printf("  %s %v\n", stErr.Render("did not work:"), err)
		}
	}
	if ran == 0 {
		fmt.Println("\nnothing here that dun can fix by itself")
		return code
	}
	fmt.Println()
	time.Sleep(time.Second) // a restarted service answers again
	return check().report()
}

// check runs every check.
func check() *doctor {
	d := &doctor{}
	d.providers()
	d.tools()
	d.dund()
	d.build()
	return d
}

// report prints the findings; 1 when one failed.
func (d *doctor) report() int {
	fmt.Printf("dun doctor — dun %s, %s/%s, home %s\n\n", version, runtime.GOOS, runtime.GOARCH, dunHome())
	failed := false
	for _, f := range d.found {
		mark := map[string]string{"ok": stTool.Render("ok  "), "warn": stDim.Render("warn"), "fail": stErr.Render("FAIL")}[f.level]
		fmt.Printf("  %s  %-10s %s\n", mark, f.area, f.detail)
		if f.fix != "" {
			fmt.Printf("  %s  %-10s %s\n", "    ", "", stDim.Render("→ "+f.fix))
		}
		failed = failed || f.level == "fail"
	}
	if failed {
		return 1
	}
	return 0
}

func (d *doctor) providers() {
	if st, err := os.Stat(configPath()); err == nil && st.Mode().Perm()&0o077 != 0 {
		d.add("warn", "config", fmt.Sprintf("%s is %v: it holds keys", configPath(), st.Mode().Perm()), "chmod 600 "+configPath())
	}
	cfg := loadConfig()
	ps := cfg.providerList()
	if len(ps) == 0 {
		d.add("fail", "provider", "not logged in to a provider: dun talks to no model",
			"/login <name> in the TUI, web or app, or `dun --setup` (an OpenAI-compatible URL, bearer optional)")
		return
	}
	def := cfg.activeName()
	if cfg.Model == "" {
		d.add("fail", "model", "no model chosen on "+def, "/model in the TUI, web or app")
	}
	for _, p := range ps {
		name := p.Name
		if name == def {
			name += " (default)"
		}
		ids, why := listModels(p.URL, p.Key)
		switch {
		case len(ids) == 0:
			d.add("warn", "provider", fmt.Sprintf("%s at %s, %s: %s", name, p.URL, keyNote(p.Key), why),
				"a wrong key: /login "+p.Name+" again; a gateway that lists nothing: /model use "+p.Name+" <id>")
		case p.Name == def && cfg.Model != "" && !slices.Contains(ids, cfg.Model):
			d.add("warn", "model", fmt.Sprintf("%s is not among %s's %d models", cfg.Model, p.Name, len(ids)), "/model to pick one it lists")
		default:
			detail := fmt.Sprintf("%s at %s, %s: %d models", name, p.URL, keyNote(p.Key), len(ids))
			if p.Name == def && cfg.Model != "" {
				detail += "; model " + cfg.Model
			}
			d.add("ok", "provider", detail, "")
		}
	}
}

// doctorTools are what the engines run: required, or what needs them.
var doctorTools = []struct{ name, need string }{
	{"git", ""},
	{"mcpshell", ""},
	{"poly-lsp-mcp", "/poly-lsp"},
	{"raglit", "/raglit"},
}

func (d *doctor) tools() {
	svcPATH, svcHasPATH := dundcli.ServicePATH(dundcli.ServiceUnit)
	_, svcInstalled := dundcli.ServiceFile(dundcli.ServiceUnit)
	for _, t := range doctorTools {
		level := "fail"
		if t.need != "" {
			level = "warn"
		}
		p, err := exec.LookPath(t.name)
		if err != nil {
			what := "not on PATH"
			if t.need != "" {
				what += " (needed only for " + t.need + ")"
			}
			d.add(level, "tool", t.name+": "+what, "install it, or put its directory on PATH")
			continue
		}
		if svcInstalled && svcHasPATH && lookIn(svcPATH, t.name) == "" {
			d.add(level, "tool", t.name+": "+p+", but not on the service's PATH",
				"reinstall the service from this shell to capture its PATH: dun dund service install -- <flags>")
			continue
		}
		d.add("ok", "tool", t.name+": "+p, "")
	}
}

// lookIn finds name in a PATH that is not this process's.
func lookIn(path, name string) string {
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (d *doctor) dund() {
	file, installed := dundcli.ServiceFile(dundcli.ServiceUnit)
	if installed {
		d.add("ok", "service", "installed: "+file, "")
		if _, ok := dundcli.ServicePATH(dundcli.ServiceUnit); !ok {
			d.add("warn", "service", "it sets no PATH: its engines get the service manager's, not yours",
				"reinstall it from your shell: dun dund service install -- <flags>")
		}
	} else {
		d.add("ok", "service", "not installed (optional: the TUI starts dund itself)", "")
	}
	sock := dundcli.Socket(dunHome())
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		fix := "start it: run dun (the TUI starts it)"
		if installed {
			fix = "dun dund service start; dun dund service logs to see why it stopped"
		}
		d.add("warn", "dund", "not running at "+sock, fix)
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(proto.NewCommand("doctor", &proto.ListSessions{})); err != nil {
		d.add("fail", "dund", "cannot write to "+sock+": "+err.Error(), "")
		return
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Re != "doctor" {
			continue
		}
		if a, ok := ev.Payload.(*proto.Ack); ok && a.Error != "" {
			d.add("fail", "dund", "refused this user: "+a.Error, "")
			return
		}
		if l, ok := ev.Payload.(*proto.SessionList); ok {
			d.add("ok", "dund", fmt.Sprintf("answers at %s: %d session(s) for this user", sock, len(l.Items)), "")
			d.dundBuild(c, sc)
			return
		}
	}
	d.add("fail", "dund", "no answer at "+sock+" (it identifies you by the kernel's peer credentials)",
		"dun dund service logs (or "+dundLog()+")")
}

// dundBuild compares the running dund's build with this binary's: a dund left
// running from an older build keeps its old behaviour (and its old bugs) until
// it restarts.
func (d *doctor) dundBuild(c net.Conn, sc *bufio.Scanner) {
	pid := filepath.Join(dunHome(), "dund.pid")
	restart := "stop it — kill $(cat " + pid + ") — and start dun again; sessions are kept"
	if _, svc := dundcli.ServiceFile(dundcli.ServiceUnit); svc {
		restart = "dun dund service restart; sessions are kept"
	}
	if err := json.NewEncoder(c).Encode(proto.NewCommand("doctor-hello", &proto.Hello{Version: version})); err != nil {
		return
	}
	for sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Re != "doctor-hello" {
			continue
		}
		r, ok := ev.Payload.(*proto.HelloReply)
		switch {
		case !ok:
			d.add("fail", "dund", "runs an older build that predates build checks (and today's fixes)", restart)
		case r.Version == version || r.Version == "dev" || version == "dev":
			d.add("ok", "dund", "runs this build ("+r.Version+")", "")
		case r.Upgradable && r.Busy:
			d.add("warn", "dund", "runs build "+r.Version+", this is "+version+", and is busy (a turn, a question or a job is running), so it cannot restart yet",
				"let it finish, interrupt it (ctrl+g), kill its jobs (/jobs kill all) — or "+restart)
		case r.Upgradable:
			d.add("warn", "dund", "runs build "+r.Version+", this is "+version,
				"start dun: it asks dund to restart into this build (or "+restart+")")
		default:
			d.add("warn", "dund", "the service runs build "+r.Version+", this is "+version, "dun dund service restart")
		}
		return
	}
}

func (d *doctor) build() {
	if srcDir == "" {
		d.add("ok", "build", "a release build (no self-update)", "")
		return
	}
	mod, err := os.ReadFile(filepath.Join(srcDir, "go.mod"))
	if err != nil {
		d.add("warn", "build", "self-updating from "+srcDir+", which is gone", "make install from a checkout")
		return
	}
	want := ""
	for _, line := range strings.Split(string(mod), "\n") {
		if v, ok := strings.CutPrefix(line, "go "); ok {
			want = strings.TrimSpace(v)
		}
	}
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		d.add("warn", "build", "self-updating from "+srcDir+", but go is not on PATH: it cannot rebuild", "install Go "+want+" or newer")
		return
	}
	have := strings.TrimPrefix(strings.TrimSpace(string(out)), "go")
	if want != "" && versionLess(have, want) {
		d.add("warn", "build", fmt.Sprintf("go %s, but %s/go.mod needs %s (it will try to download it)", have, srcDir, want),
			"install Go "+want+" or newer")
		return
	}
	d.add("ok", "build", fmt.Sprintf("self-updating from %s with go %s", srcDir, have), "")
}

// versionLess compares dotted versions numerically ("1.26.2" < "1.26.10").
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}
