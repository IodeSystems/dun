package dun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startSetup starts a session whose code server is the fake poly-lsp
// sending the setup notification for mode, and returns the harness and the
// lines dun said about it.
func startSetup(t *testing.T, mode string) (*Harness, chan string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSetupEnv+"="+mode)
	lines := make(chan string, 8)
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers:       []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
		OnServerSetup: func(id, msg string) { lines <- msg },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, lines
}

// waitSetup waits for the code server's setup report to be kept.
func waitSetup(t *testing.T, h *Harness) *SetupReport {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r := codeState(h).Setup; r != nil {
			return r
		}
	}
	t.Fatal("no setup report")
	return nil
}

func noLine(t *testing.T, lines chan string) {
	t.Helper()
	select {
	case l := <-lines:
		t.Errorf("said %q", l)
	case <-time.After(300 * time.Millisecond):
	}
}

// fakeToolchains makes only the named programs be on PATH.
func fakeToolchains(t *testing.T, have ...string) {
	t.Helper()
	old := onPath
	onPath = func(bin string) bool {
		for _, h := range have {
			if h == bin {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { onPath = old })
}

// A Go-heavy root without gopls: the report is kept on the server, and one
// line says what gopls adds and how to get it.
func TestSetup_MissingServerInAHeavyLanguageIsSaid(t *testing.T) {
	fakeToolchains(t, "go")
	h, lines := startSetup(t, "go")
	rep := waitSetup(t, h)
	if rep.Missing != 1 || len(rep.Languages) != 2 || rep.Languages[0].LSP.Name != "gopls" || rep.Languages[1].LSP != nil {
		t.Fatalf("report: %+v", rep)
	}
	want := "poly-lsp: gopls would add references, rename and diagnostics for Go (412 files) — go install golang.org/x/tools/gopls@latest"
	select {
	case got := <-lines:
		if got != want {
			t.Errorf("line:\n got %q\nwant %q", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no line")
	}
	// A second report in the same session is kept, and not said again.
	if l := h.noteSetup(ServerCode, *rep); l != "" {
		t.Errorf("said twice: %q", l)
	}
	noLine(t, lines)
}

// 12 Python files among 412 Go: a missing pylsp is on the server's state
// and in /poly-lsp, and says nothing in the conversation.
func TestSetup_SmallLanguageStaysQuiet(t *testing.T) {
	h, lines := startSetup(t, "python")
	if rep := waitSetup(t, h); rep.Missing != 1 {
		t.Fatalf("report: %+v", rep)
	}
	noLine(t, lines)
}

func TestSetup_NothingMissingStaysQuiet(t *testing.T) {
	h, lines := startSetup(t, "ok")
	if rep := waitSetup(t, h); rep.Missing != 0 {
		t.Fatalf("report: %+v", rep)
	}
	noLine(t, lines)
}

// An older poly-lsp sends no setup report: no state, no line.
func TestSetup_OlderServerChangesNothing(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp")
	lines := make(chan string, 1)
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers:       []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
		OnServerSetup: func(id, msg string) { lines <- msg },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	time.Sleep(300 * time.Millisecond)
	if st := codeState(h); st.Setup != nil || !st.Running {
		t.Errorf("state: %+v", st)
	}
	noLine(t, lines)
}

func testLang(name string, files int, lsp string, found bool, cmd string, needs ...string) SetupLang {
	l := SetupLang{Lang: name, Files: files}
	if lsp != "" {
		l.LSP = &SetupLSP{Name: lsp, Found: found}
		if !found && cmd != "" {
			l.Install = &SetupInstall{Cmd: cmd, Needs: needs}
		}
	}
	return l
}

// The share rule: a tenth of the listed files, or 200 files.
func TestSetup_ShareRule(t *testing.T) {
	fakeToolchains(t, "go", "node", "python3")
	py := func(n int) SetupLang {
		return testLang("python", n, "pylsp", false, "pipx install python-lsp-server", "python")
	}
	gopls := func(n int) SetupLang { return testLang("go", n, "gopls", true, "") }
	for _, c := range []struct {
		name  string
		langs []SetupLang
		want  bool
	}{
		{"dun's own tree: 12 of 1,300", []SetupLang{gopls(1288), py(12)}, false},
		{"just under a tenth", []SetupLang{gopls(1000), py(110)}, false},
		{"a tenth", []SetupLang{gopls(900), py(100)}, true},
		{"a small project, a fifth", []SetupLang{gopls(48), py(12)}, true},
		{"200 files in a huge one", []SetupLang{gopls(40000), py(200)}, true},
		{"199 files in a huge one", []SetupLang{gopls(40000), py(199)}, false},
		{"tree-sitter only is never missing", []SetupLang{testLang("markdown", 5000, "", false, ""), gopls(1)}, false},
	} {
		got := SetupLine(SetupReport{Languages: c.langs}, "poly-lsp")
		if (got != "") != c.want {
			t.Errorf("%s: line %q, want a line: %v", c.name, got, c.want)
		}
	}
}

func TestSetup_LineNamesTheToolchainAndTheSeveral(t *testing.T) {
	fakeToolchains(t, "go")
	ts := testLang("typescript", 230, "typescript-language-server", false, "npm install -g typescript-language-server typescript", "node")
	one := SetupLine(SetupReport{Languages: []SetupLang{ts}}, "poly-lsp")
	want := "poly-lsp: typescript-language-server would add references, rename and diagnostics for TypeScript (230 files) — npm install -g typescript-language-server typescript (needs Node)"
	if one != want {
		t.Errorf("one:\n got %q\nwant %q", one, want)
	}
	gopls := testLang("go", 412, "gopls", false, "go install golang.org/x/tools/gopls@latest", "go")
	two := SetupLine(SetupReport{Languages: []SetupLang{gopls, ts}}, "poly-lsp")
	want = "poly-lsp: gopls and typescript-language-server would add references, rename and diagnostics for Go (412 files) and TypeScript (230 files) — /poly-lsp has the install commands"
	if two != want {
		t.Errorf("two:\n got %q\nwant %q", two, want)
	}
	noHint := testLang("go", 412, "gopls", false, "")
	if got := SetupLine(SetupReport{Languages: []SetupLang{noHint}}, "poly-lsp"); !strings.HasSuffix(got, "(412 files) — /poly-lsp") {
		t.Errorf("no install hint: %q", got)
	}
}

func TestSetup_Table(t *testing.T) {
	fakeToolchains(t, "go")
	rep := SetupReport{Languages: []SetupLang{
		{Lang: "go", Files: 412, LSP: &SetupLSP{Name: "gopls", Found: true, Path: "/usr/bin/gopls"}},
		{Lang: "typescript", Files: 230, LSP: &SetupLSP{Name: "typescript-language-server"}, Adds: "resolved references",
			Install: &SetupInstall{Cmd: "npm install -g typescript-language-server typescript", Needs: []string{"node"}}},
		{Lang: "markdown", Files: 40},
	}}
	got := SetupTable(rep)
	for _, w := range []string{
		"  Go · 412 files · gopls found at /usr/bin/gopls",
		"  TypeScript · 230 files · typescript-language-server missing",
		"    adds: resolved references",
		"    install: npm install -g typescript-language-server typescript · needs node (not on PATH: Node)",
		"  Markdown · 40 files · tree-sitter only",
		"dun does not install them",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("table lacks %q:\n%s", w, got)
		}
	}
}

// doctor --json's languages are the notification's shape.
func TestSetup_ParseDoctorJSON(t *testing.T) {
	sample := `{"binary":"/x/poly-lsp-mcp","build":"b","root":"/p","languages":[
	  {"lang":"go","files":412,"lsp":{"name":"gopls","found":false},"adds":"a","install":{"cmd":"go install golang.org/x/tools/gopls@latest","needs":["go"]}},
	  {"lang":"python","files":12,"lsp":{"name":"pylsp","found":true,"path":"/usr/bin/pylsp"}},
	  {"lang":"markdown","files":40}],"problems":["go: 412 file(s) but gopls is not on PATH"]}`
	rep, ok := ParseDoctorLanguages([]byte(sample))
	if !ok || len(rep.Languages) != 3 || rep.Missing != 1 || rep.MissingLSPs()[0].Install.Needs[0] != "go" {
		t.Fatalf("%v %+v", ok, rep)
	}
	for _, bad := range []string{"", "Error: unknown command \"doctor\"", `{"binary":"x"}`} {
		if _, ok := ParseDoctorLanguages([]byte(bad)); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
