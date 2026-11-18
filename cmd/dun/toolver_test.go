package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// The decision table: where the tool comes from (pinned, a kept older
// copy, your build) × its version against this dun's × what this dun and
// machine can do. Each row is the line, the fix, and what --fix runs.
func TestPlanTool(t *testing.T) {
	s := release.Tool{Name: "t", Module: "github.com/x/t/cmd/t", Min: "v0.2.0", Recommended: "v0.3.0"}
	gi := func(v string) update.Origin { return update.Origin{Kind: update.GoInstall, Version: v} }
	rel := func(v string) update.Origin { return update.Origin{Kind: update.Release, Version: v} }
	co := func(v string, mod bool) update.Origin {
		return update.Origin{Kind: update.Checkout, Version: v, Revision: "abc1234", Modified: mod}
	}
	pinned := func(v string) mcpserver.Resolved {
		return mcpserver.Resolved{Name: "t", Source: mcpserver.SourcePinned, Path: "/h/tools/t/" + v + "/t", Pin: "v0.3.0", Version: v}
	}
	env := mcpserver.Resolved{Name: "t", Source: mcpserver.SourceOverride, Path: "/src/t", Via: "$DUN_T", Pin: "v0.3.0"}
	cfg := mcpserver.Resolved{Name: "t", Source: mcpserver.SourceOverride, Path: "/src/t", Via: "tools.t.path in /h/mcp.json", Pin: "v0.3.0"}
	goOK, rl := toolEnv{haveGo: true}, toolEnv{release: true}
	for i, c := range []struct {
		r                       mcpserver.Resolved
		o                       update.Origin
		env                     toolEnv
		level, detail, fix, act string
	}{
		// the pin's copy: nothing to do, whatever its origin says
		{pinned("v0.3.0"), rel("v0.3.0"), rl, "ok", "t: pinned v0.3.0, release v0.3.0", "", ""},
		{pinned("v0.3.0"), gi("v0.3.0"), goOK, "ok", "t: pinned v0.3.0, go install v0.3.0", "", ""},
		// a kept older copy: --fix gets the pin's
		{pinned("v0.2.5"), rel("v0.2.5"), rl, "ok", "t: pinned v0.2.5, release v0.2.5; a kept copy: this dun pins v0.3.0",
			"dun doctor --fix (downloads t v0.3.0 from its release; the sha256 is pinned in this build)", actDownload},
		{pinned("v0.2.5"), gi("v0.2.5"), goOK, "ok", "t: pinned v0.2.5, go install v0.2.5; a kept copy: this dun pins v0.3.0",
			"dun doctor --fix (runs go install github.com/x/t/cmd/t@v0.3.0)", actGoInstall},
		{pinned("v0.2.5"), gi("v0.2.5"), toolEnv{}, "ok", "t: pinned v0.2.5, go install v0.2.5; a kept copy: this dun pins v0.3.0",
			"install Go (go.dev/dl), then dun doctor --fix", ""},
		// your build: run whatever its version; below Min is a warn, never updated
		{env, gi("v0.3.0"), goOK, "ok", "t: your build, go install v0.3.0", "", ""},
		{env, gi("v0.2.5"), goOK, "ok", "t: your build, go install v0.2.5", "", ""},
		{env, gi("v0.1.0"), goOK, "warn", "t: your build, go install v0.1.0; too old for this dun (it needs v0.2.0 or newer)",
			"update it, or unset DUN_T to run the pinned v0.3.0", ""},
		{cfg, rel("v0.1.0"), rl, "warn", "t: your build, release v0.1.0; too old for this dun (it needs v0.2.0 or newer)",
			"update it, or remove tools.t.path from " + mcpserver.UserServersFile() + " to run the pinned v0.3.0", ""},
		// a modified checkout: compared by its build info's version (Carl's Mac, 2026-11-09)
		{env, co("v0.1.2-0.20261030062958-886d6f12c3e4+dirty", true), goOK, "warn",
			"t: your build, built from a checkout at abc1234, modified; too old for this dun (it needs v0.2.0 or newer)",
			"update it, or unset DUN_T to run the pinned v0.3.0", ""},
		{env, co("v0.3.1-0.20261030062958-886d6f12c3e4+dirty", true), goOK, "ok", "t: your build, built from a checkout at abc1234, modified", "", ""},
		// no version to compare
		{env, co("v0.0.0-20260101000000-abc123456789", false), goOK, "ok",
			"t: your build, built from a checkout at abc1234; unknown version, can't compare it with v0.2.0 (the oldest this dun runs)", "", ""},
		{env, update.Origin{Kind: update.GoBuild, Version: "(devel)"}, goOK, "ok",
			"t: your build, a plain go build, outside version control; unknown version, can't compare it with v0.2.0 (the oldest this dun runs)", "", ""},
	} {
		pl := planTool("t", c.r, c.o, s, c.env)
		if pl.level != c.level || pl.detail != c.detail || pl.fix != c.fix || pl.act != c.act {
			t.Errorf("row %d: got\n  %s | %s | %s | %s\nwant\n  %s | %s | %s | %s", i,
				pl.level, pl.detail, pl.fix, pl.act, c.level, c.detail, c.fix, c.act)
		}
	}
}

// The go install modules agree with dun's ToolInstall (what a missing
// server's message names).
func TestSupportedModules(t *testing.T) {
	for _, s := range release.Supported {
		inst, ok := mcpserver.ToolInstall[s.Name]
		if m, _, _ := strings.Cut(inst.Module, "@"); !ok || m != s.Module {
			t.Errorf("%s: ToolInstall %q, Supported %q", s.Name, inst.Module, s.Module)
		}
	}
}

// poly-lsp's own update report, relayed as one line, or nothing.
func TestPolyUpdate(t *testing.T) {
	for out, want := range map[string]string{
		`{"languages":[]}`: "",
		`not json`:         "",
		`{"update":{"origin":"go install","current":"v0.9.2","latest":"v0.9.4","available":true,"how":"go install github.com/iodesystems/poly-lsp-mcp@latest"}}`: "ok poly-lsp-mcp says an update is available: v0.9.2 → v0.9.4 | go install github.com/iodesystems/poly-lsp-mcp@latest",
		`{"update":{"origin":"go install","current":"v0.9.4","latest":"v0.9.4","available":false,"note":"cached 3h ago"}}`:                                       "",
		`{"update":{"origin":"source","current":"a019fc7+dirty","note":"modified checkout: not checking"}}`:                                                      "ok poly-lsp-mcp a019fc7+dirty: modified checkout: not checking |",
		`{"update":{"origin":"release","current":"v1"}}`:                                                                                                         "",
	} {
		d := &doctor{}
		d.addToolUpdate("poly-lsp-mcp", parsePolyUpdate([]byte(out)))
		got := ""
		for _, f := range d.found {
			got = strings.TrimSpace(f.level + " " + f.detail + " | " + f.fix)
		}
		if got != want {
			t.Errorf("%s:\n got %q\nwant %q", out, got, want)
		}
	}
}

// Real binaries: go install builds at a module version (from a proxy in a
// temp dir, no network), read by toolOrigin from their build info. Your
// build below Min is a warn and never touched; a kept older copy is
// updated by --fix with go install module@pin into the pin's directory
// under DUN_HOME.
func TestToolOrigin_RealBinaries(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	if testing.Short() {
		t.Skip("builds binaries")
	}
	const mod = "example.com/faketool"
	proxy := t.TempDir()
	for _, v := range []string{"v0.1.0", "v0.2.5", "v0.3.0"} {
		fakeModule(t, proxy, mod, v)
	}
	gobin := func(dir string) []string {
		return append(os.Environ(), "GOPROXY=file://"+filepath.ToSlash(proxy), "GOSUMDB=off", "GOFLAGS=-modcacherw",
			"GOMODCACHE="+filepath.Join(t.TempDir(), "mod"), "GOBIN="+dir, "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	}
	install := func(v, dir string) string {
		t.Helper()
		cmd := exec.Command("go", "install", mod+"@"+v)
		cmd.Dir = t.TempDir()
		cmd.Env = gobin(dir)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go install %s@%s: %v\n%s", mod, v, err, b)
		}
		return filepath.Join(dir, "faketool")
	}
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_FAKETOOL", "")
	defer func(s []release.Tool, g func(string, string) (string, error)) {
		release.Supported, goInstall = s, g
	}(release.Supported, goInstall)
	release.Supported = []release.Tool{{Name: "faketool", Module: mod, Min: "v0.2.0", Recommended: "v0.3.0"}}

	line := func(d *doctor) finding {
		t.Helper()
		if len(d.found) != 1 {
			t.Fatalf("%+v", d.found)
		}
		return d.found[0]
	}
	// Your build, too old: said, never updated.
	old := install("v0.1.0", t.TempDir())
	t.Setenv("DUN_FAKETOOL", old)
	goInstall = func(string, string) (string, error) { t.Error("go install for your build"); return "", nil }
	d := &doctor{fix: true}
	d.toolOrigin("faketool", mcpserver.ResolveTool("faketool"))
	if f := line(d); f.level != "warn" || f.detail != "faketool: your build, go install v0.1.0; too old for this dun (it needs v0.2.0 or newer)" ||
		f.fix != "update it, or unset DUN_FAKETOOL to run the pinned v0.3.0" {
		t.Errorf("your build: %+v", f)
	}

	// A kept v0.2.5 copy: --fix go-installs v0.3.0 into its own directory.
	t.Setenv("DUN_FAKETOOL", "")
	install("v0.2.5", filepath.Dir(mcpserver.PinnedPath("faketool", "v0.2.5")))
	r := mcpserver.ResolveTool("faketool")
	if !r.Stale() || r.Version != "v0.2.5" {
		t.Fatalf("kept: %+v", r)
	}
	var ran []string
	goInstall = func(module, dir string) (string, error) {
		ran = append(ran, module)
		cmd := exec.Command("go", "install", module)
		cmd.Dir = t.TempDir()
		cmd.Env = gobin(dir)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("%s", b)
		}
		return dir, err
	}
	d = &doctor{fix: true}
	d.toolOrigin("faketool", r)
	if f := line(d); f.level != "ok" || f.detail != "faketool: pinned v0.3.0, go install v0.3.0" || f.fix != "" ||
		len(ran) != 1 || ran[0] != mod+"@v0.3.0" || d.applied != 1 {
		t.Errorf("--fix: %+v ran %q", f, ran)
	}
	if r := mcpserver.ResolveTool("faketool"); r.Path != mcpserver.PinnedPath("faketool", "v0.3.0") || r.Stale() {
		t.Errorf("after --fix: %+v", r)
	}
	if _, err := os.Stat(mcpserver.PinnedPath("faketool", "v0.2.5")); err != nil {
		t.Error("the old version directory is kept")
	}
}

// fakeModule puts module mod at version v in a file GOPROXY at proxy: a
// main package that does nothing.
func fakeModule(t *testing.T, proxy, mod, v string) {
	t.Helper()
	src := t.TempDir()
	gomod := "module " + mod + "\n\ngo 1.22\n"
	os.WriteFile(filepath.Join(src, "go.mod"), []byte(gomod), 0o644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	dir := filepath.Join(proxy, mod, "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, v+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	if err := modzip.CreateFromDir(f, module.Version{Path: mod, Version: v}, src); err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.WriteFile(filepath.Join(dir, v+".mod"), []byte(gomod), 0o644)
	os.WriteFile(filepath.Join(dir, v+".info"), []byte(`{"Version":"`+v+`","Time":"2026-01-01T00:00:00Z"}`), 0o644)
	l, _ := os.OpenFile(filepath.Join(dir, "list"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	l.WriteString(v + "\n")
	l.Close()
}

// A missing tool's install step names the version this dun was tested with,
// never @latest (the proxy caches it).
func TestToolInstallIsPinned(t *testing.T) {
	for _, s := range release.Supported {
		if m := mcpserver.ToolInstall[s.Name].Module; m != s.Module+"@"+s.Recommended {
			t.Errorf("%s: ToolInstall %q, want @%s", s.Name, m, s.Recommended)
		}
	}
}

// The session-start warning: your build below Min says so, with the path,
// version and fix; at or past Min it says nothing (it is yours). A kept
// copy behind the pin says so on a build that does not fetch; a release
// build fetches the pin itself. A version with no tag is unknown, not old.
func TestToolWarning(t *testing.T) {
	s, _ := release.SupportedTool("mcpshell")
	const p = "/home/u/.local/bin/mcpshell"
	mine := mcpserver.Resolved{Name: "mcpshell", Source: mcpserver.SourceOverride, Path: p, Via: "$DUN_MCPSHELL", Pin: s.Recommended}
	const old = "v0.1.2-0.20261030062958-886d6f12c3e4"
	carl := update.Origin{Kind: update.Checkout, Version: old + "+dirty", Revision: "886d6f1", Modified: true}
	want := "mcpshell: your build at " + p + " ($DUN_MCPSHELL) is " + old + "+dirty, older than the " + s.Min +
		" this dun needs. Fix: update it, or unset DUN_MCPSHELL to run the pinned " + s.Recommended
	if got := toolWarning("mcpshell", mine, carl, s, toolEnv{}); got != want {
		t.Errorf("old modified checkout:\n got %q\nwant %q", got, want)
	}
	for _, o := range []update.Origin{
		{Kind: update.Release, Version: s.Min},
		{Kind: update.GoInstall, Version: "v0.7.2"}, // behind the pin, past Min: yours
		{Kind: update.Checkout, Version: "v99.0.1-0.20261111161500-ca705850cc73+dirty", Revision: "ca70585", Modified: true},
		{Kind: update.Checkout, Version: "(devel)", Revision: "abc1234", Modified: true},
		{Kind: update.GoBuild, Version: "(devel)"},
		{Kind: update.Checkout, Version: "v0.0.0-20261101000000-abcdefabcdef", Revision: "abc1234"},
	} {
		if got := toolWarning("mcpshell", mine, o, s, toolEnv{}); got != "" {
			t.Errorf("%+v: %q", o, got)
		}
	}
	kept := mcpserver.Resolved{Name: "mcpshell", Source: mcpserver.SourcePinned, Path: "/h/tools/mcpshell/v0.7.2/mcpshell", Pin: s.Recommended, Version: "v0.7.2"}
	o := update.Origin{Kind: update.Release, Version: "v0.7.2"}
	if got := toolWarning("mcpshell", kept, o, s, toolEnv{haveGo: true}); !strings.Contains(got, "running the kept v0.7.2 at "+kept.Path+"; this dun pins "+s.Recommended) {
		t.Errorf("kept, non-release: %q", got)
	}
	if got := toolWarning("mcpshell", kept, o, s, toolEnv{release: true}); got != "" {
		t.Errorf("kept, release: %q", got)
	}
}
