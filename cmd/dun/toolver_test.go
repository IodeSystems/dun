package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// The decision table: the tool's origin × its version against this dun's ×
// what this dun and machine can do. Each row is the line, the fix, and
// what --fix runs.
func TestPlanTool(t *testing.T) {
	s := release.Tool{Name: "t", Module: "github.com/x/t/cmd/t", Min: "v0.2.0", Recommended: "v0.3.0"}
	gi := func(v string) update.Origin { return update.Origin{Kind: update.GoInstall, Version: v} }
	rel := func(v string) update.Origin { return update.Origin{Kind: update.Release, Version: v} }
	co := func(v, src string, mod bool) update.Origin {
		return update.Origin{Kind: update.Checkout, Version: v, Revision: "abc1234", SrcDir: src, Modified: mod}
	}
	goOK := toolEnv{haveGo: true, writable: true}
	for i, c := range []struct {
		o                       update.Origin
		env                     toolEnv
		level, detail, fix, act string
	}{
		// go install
		{gi("v0.3.0"), goOK, "ok", "t: go install v0.3.0", "", ""},
		{gi("v0.4.0"), goOK, "ok", "t: go install v0.4.0", "", ""}, // newer: never a downgrade
		{gi("v0.2.5"), goOK, "ok", "t: go install v0.2.5; an update is available: v0.3.0 (tested with this dun)",
			"dun doctor --fix (runs go install github.com/x/t/cmd/t@v0.3.0)", actGoInstall},
		{gi("v0.1.0"), goOK, "warn", "t: go install v0.1.0; too old for this dun (it needs v0.2.0 or newer)",
			"dun doctor --fix (runs go install github.com/x/t/cmd/t@v0.3.0)", actGoInstall},
		{gi("v0.1.0"), toolEnv{writable: true}, "warn", "t: go install v0.1.0; too old for this dun (it needs v0.2.0 or newer)",
			"go install github.com/x/t/cmd/t@v0.3.0 (needs Go: go.dev/dl)", ""},
		{gi("v0.2.5"), toolEnv{haveGo: true}, "ok", "t: go install v0.2.5; an update is available: v0.3.0 (tested with this dun)",
			"go install github.com/x/t/cmd/t@v0.3.0, as a user who can write /bin", ""},
		{gi("v0.3.0-0.20261001000000-abcdefabcdef"), goOK, "ok", // a pseudo-version before v0.3.0
			"t: go install v0.3.0-0.20261001000000-abcdefabcdef; an update is available: v0.3.0 (tested with this dun)",
			"dun doctor --fix (runs go install github.com/x/t/cmd/t@v0.3.0)", actGoInstall},
		// release: the release dun's pin, beside dun or not; a dun with no pin
		{rel("v0.2.5"), toolEnv{pinned: "v0.3.0", beside: true}, "ok", "t: release v0.2.5; an update is available: v0.3.0 (tested with this dun)",
			"dun doctor --fix (downloads t v0.3.0 from its release; the sha256 is pinned in this build)", actDownload},
		{rel("v0.1.0"), toolEnv{pinned: "v0.3.0"}, "warn", "t: release v0.1.0; too old for this dun (it needs v0.2.0 or newer)",
			"remove /bin/t, then dun doctor --fix downloads t v0.3.0 beside dun", ""},
		{rel("v0.2.5"), goOK, "ok", "t: release v0.2.5; an update is available: v0.3.0 (tested with this dun)",
			"download v0.3.0 or newer from https://github.com/x/t/releases", ""},
		{rel("v0.3.0"), toolEnv{pinned: "v0.3.0", beside: true}, "ok", "t: release v0.3.0", "", ""},
		{rel("(devel)"), toolEnv{pinned: "v0.3.0", beside: true}, "ok", "t: release (devel)", "", ""},
		// checkout: said, never run
		{co("v0.0.0-20260101000000-abc123456789", "/src/t", false), goOK, "warn",
			"t: built from a checkout at abc1234; too old for this dun (it needs v0.2.0 or newer)", "rebuild it in /src/t (make install)", ""},
		{co("v0.2.1-0.20260101000000-abc123456789", "", false), goOK, "ok",
			"t: built from a checkout at abc1234; an update is available: v0.3.0 (tested with this dun)", "rebuild it from its checkout (make install)", ""},
		{co("v0.3.1-0.20261101000000-abc123456789", "/src/t", false), goOK, "ok", "t: built from a checkout at abc1234", "", ""},
		// a modified checkout: compared by its build info's version (Carl's Mac, 2026-11-09)
		{co("v0.0.0-20260101000000-abc123456789+dirty", "/src/t", true), goOK, "warn",
			"t: a modified checkout of v0.0.0-20260101000000-abc123456789+dirty (at abc1234); too old for this dun (it needs v0.2.0 or newer)",
			"rebuild it in /src/t from a current tag (make install), or remove /bin/t and run dun doctor --fix to get the pinned release", ""},
		{co("v0.1.2-0.20261030062958-886d6f12c3e4+dirty", "", true), goOK, "warn",
			"t: a modified checkout of v0.1.2-0.20261030062958-886d6f12c3e4+dirty (at abc1234); too old for this dun (it needs v0.2.0 or newer)",
			"rebuild it in its checkout from a current tag (make install), or remove /bin/t and run dun doctor --fix to get the pinned release", ""},
		{co("v0.2.5-0.20261030062958-886d6f12c3e4+dirty", "", true), goOK, "warn", // modified and behind
			"t: a modified checkout of v0.2.5-0.20261030062958-886d6f12c3e4+dirty (at abc1234); an update is available: v0.3.0 (tested with this dun)",
			"rebuild it in its checkout from a current tag (make install), or remove /bin/t and run dun doctor --fix to get the pinned release", ""},
		{co("v0.3.1-0.20261030062958-886d6f12c3e4+dirty", "", true), goOK, "ok", "t: built from a checkout at abc1234, modified", "", ""},
		{co("(devel)", "/src/t", false), goOK, "ok", "t: built from a checkout at abc1234; can't compare its version with v0.3.0 (this dun's)", "", ""},
		// a plain go build
		{update.Origin{Kind: update.GoBuild, Version: "(devel)"}, goOK, "ok",
			"t: a plain go build, outside version control; can't compare its version with v0.3.0 (this dun's)", "", ""},
	} {
		pl := planTool("t", "/bin/t", c.o, s, c.env)
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
		inst, ok := dun.ToolInstall[s.Name]
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

// Real binaries: a go install build at a module version (from a proxy in a
// temp dir, no network) and a make install build from a checkout, read by
// toolOrigin from their build info. --fix go-installs the recommended
// version into the tool's own directory, never downgrades, and never
// touches a checkout's build.
func TestToolOrigin_RealBinaries(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	if testing.Short() {
		t.Skip("builds binaries")
	}
	const mod = "example.com/faketool"
	proxy := t.TempDir()
	for _, v := range []string{"v0.1.0", "v0.3.0"} {
		fakeModule(t, proxy, mod, v)
	}
	install := func(v string) string {
		t.Helper()
		dir := t.TempDir()
		cmd := exec.Command("go", "install", mod+"@"+v)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "GOPROXY=file://"+filepath.ToSlash(proxy), "GOSUMDB=off", "GOFLAGS=-modcacherw",
			"GOMODCACHE="+filepath.Join(t.TempDir(), "mod"), "GOBIN="+dir, "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go install %s@%s: %v\n%s", mod, v, err, b)
		}
		return filepath.Join(dir, "faketool")
	}
	old, cur := install("v0.1.0"), install("v0.3.0")

	defer func(s []release.Tool, g func(string, string) (string, error), p func(string) (update.Artifact, update.File, bool)) {
		release.Supported, goInstall, pinnedTool = s, g, p
	}(release.Supported, goInstall, pinnedTool)
	release.Supported = []release.Tool{{Name: "faketool", Module: mod, Min: "v0.2.0", Recommended: "v0.3.0"}}
	pinnedTool = func(string) (update.Artifact, update.File, bool) { return update.Artifact{}, update.File{}, false }

	line := func(d *doctor) finding {
		t.Helper()
		if len(d.found) != 1 {
			t.Fatalf("%+v", d.found)
		}
		return d.found[0]
	}
	// Without --fix: too old, and the fix says the command.
	d := &doctor{}
	d.toolOrigin("faketool", old)
	if f := line(d); f.level != "warn" || f.detail != "faketool: go install v0.1.0; too old for this dun (it needs v0.2.0 or newer)" ||
		f.fix != "dun doctor --fix (runs go install example.com/faketool@v0.3.0)" {
		t.Errorf("old: %+v", f)
	}
	// --fix: go install module@recommended into the tool's directory; the
	// line after is the new build's.
	var ran []string
	goInstall = func(module, dir string) (string, error) {
		ran = append(ran, module+" "+dir)
		b, err := os.ReadFile(cur)
		if err != nil {
			return "", err
		}
		return dir, os.WriteFile(filepath.Join(dir, "faketool"), b, 0o755)
	}
	d = &doctor{fix: true}
	d.toolOrigin("faketool", old)
	if f := line(d); f.level != "ok" || f.detail != "faketool: go install v0.3.0" || f.fix != "" ||
		len(ran) != 1 || ran[0] != mod+"@v0.3.0 "+filepath.Dir(old) || d.applied != 1 {
		t.Errorf("--fix: %+v ran %q", f, ran)
	}
	// Current, and newer than recommended: nothing to run.
	release.Supported[0].Recommended, release.Supported[0].Min = "v0.2.0", "v0.1.0"
	ran = nil
	d = &doctor{fix: true}
	d.toolOrigin("faketool", cur)
	if f := line(d); f.level != "ok" || f.detail != "faketool: go install v0.3.0" || len(ran) != 0 {
		t.Errorf("no downgrade: %+v ran %q", f, ran)
	}

	// A make install build from a checkout: said, never rebuilt.
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	release.Supported[0].Recommended, release.Supported[0].Min = "v0.3.0", "v0.2.0"
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+mod+"\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "x"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = src
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	co := filepath.Join(t.TempDir(), "faketool")
	cmd := exec.Command("go", "build", "-o", co, "-ldflags", "-X main.srcDir="+src, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	goInstall = func(string, string) (string, error) {
		t.Error("go install for a checkout's build")
		return "", errors.New("no")
	}
	d = &doctor{fix: true}
	d.toolOrigin("faketool", co)
	if f := line(d); f.level != "warn" || !strings.HasPrefix(f.detail, "faketool: built from a checkout at ") ||
		!strings.HasSuffix(f.detail, "; too old for this dun (it needs v0.2.0 or newer)") || f.fix != "rebuild it in "+src+" (make install)" {
		t.Errorf("checkout: %+v", f)
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
		if m := dun.ToolInstall[s.Name].Module; m != s.Module+"@"+s.Recommended {
			t.Errorf("%s: ToolInstall %q, want @%s", s.Name, m, s.Recommended)
		}
	}
}

// The session-start warning: Carl's Mac ran a modified mcpshell checkout
// found on PATH first (2026-11-09); doctor warns, and the start says so
// with the path, version, pin and fix. A release at the pin says nothing,
// and a build with no version is not compared.
func TestToolWarning(t *testing.T) {
	s, _ := release.SupportedTool("mcpshell")
	const v = "v0.7.2-0.20261030062958-886d6f12c3e4+dirty"
	const p = "/Users/carltaylor/.local/bin/mcpshell"
	carl := update.Origin{Kind: update.Checkout, Version: v, Revision: "886d6f1", Modified: true}
	env := toolEnv{pinned: s.Recommended}
	if pl := planTool("mcpshell", p, carl, s, env); pl.level != "warn" {
		t.Errorf("doctor on Carl's build: %s %s", pl.level, pl.detail)
	}
	want := "mcpshell: " + p + " is " + v + ", not a release build; this dun pins " + s.Recommended +
		". Fix: rebuild it in its checkout from a current tag (make install), or remove " + p +
		" and run dun doctor --fix to get the pinned release"
	if got := toolWarning("mcpshell", p, carl, s, env); got != want {
		t.Errorf("Carl's build:\n got %q\nwant %q", got, want)
	}
	old := update.Origin{Kind: update.Release, Version: "v0.1.0"}
	if got := toolWarning("mcpshell", p, old, s, env); !strings.Contains(got, "is v0.1.0, too old for this dun (it needs "+s.Min+" or newer); this dun pins "+s.Recommended) {
		t.Errorf("a release below Min: %q", got)
	}
	pseudo := update.Origin{Kind: update.GoInstall, Version: "v0.11.1-0.20261108000000-abcdefabcdef"}
	if got := toolWarning("mcpshell", p, pseudo, s, env); !strings.Contains(got, "not a release build") {
		t.Errorf("a pseudo-version: %q", got)
	}
	for _, o := range []update.Origin{
		{Kind: update.Release, Version: s.Recommended},
		{Kind: update.GoInstall, Version: s.Recommended},
		{Kind: update.Checkout, Version: "(devel)", Revision: "abc1234", Modified: true},
		{Kind: update.GoBuild, Version: "(devel)"},
	} {
		if got := toolWarning("mcpshell", p, o, s, env); got != "" {
			t.Errorf("%+v: %q", o, got)
		}
	}
}
