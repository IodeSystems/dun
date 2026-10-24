package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// Install hints (dun/setupstate.go) in /mcp info and in doctor.

// /mcp info's poly-lsp row counts the language servers it is missing, and
// says nothing of a server that sent no report or has none missing.
func TestMCPInfo_LanguageServerMissing(t *testing.T) {
	miss := func(n int) *dun.SetupReport {
		r := &dun.SetupReport{Languages: []dun.SetupLang{{Lang: "markdown", Files: 3}}}
		for i := 0; i < n; i++ {
			r.Languages = append(r.Languages, dun.SetupLang{Lang: "go", Files: 9, LSP: &dun.SetupLSP{Name: "gopls"}})
		}
		return r
	}
	for _, c := range []struct {
		setup *dun.SetupReport
		want  string
	}{
		{miss(1), "  poly-lsp  running · built-in · 4 tools · ~300 tokens · 1 language server missing\n"},
		{miss(2), "  poly-lsp  running · built-in · 4 tools · ~300 tokens · 2 language servers missing\n"},
		{miss(0), "  poly-lsp  running · built-in · 4 tools · ~300 tokens\n"},
		{nil, "  poly-lsp  running · built-in · 4 tools · ~300 tokens\n"},
	} {
		srv := []dun.ServerState{{ID: "poly-lsp", Running: true, Auto: true, Tools: 4, Setup: c.setup}}
		s := plainInfo(mcpInfoIn{servers: srv, parts: []dun.ContextPart{{Name: "mcp: poly-lsp", Tokens: 300}}, counted: true})
		if !strings.Contains(s, c.want) {
			t.Errorf("want %q in:\n%s", c.want, s)
		}
	}
}

// An install hint is not "a server came up in the background": it stays in
// the conversation.
func TestSplashSays_NotTheSetupHint(t *testing.T) {
	m := tuiModel{}
	if m.splashSays(evMsg{"background": true, "action": "setup", "message": "poly-lsp: gopls would add…"}) {
		t.Error("the splash swallowed the install hint")
	}
}

// doctor reads `poly-lsp-mcp doctor --json`: a warn per missing language
// server with its install command; tree-sitter only and found are fine.
func TestDoctorLanguages(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	newer := write("new", `if [ "$1 $2" = "doctor --json" ]; then cat <<'EOF'
{"binary":"x","languages":[
 {"lang":"go","files":412,"lsp":{"name":"gopls","found":false},"adds":"a","install":{"cmd":"go install golang.org/x/tools/gopls@latest","needs":["go"]}},
 {"lang":"python","files":12,"lsp":{"name":"pylsp","found":true,"path":"/usr/bin/pylsp"}},
 {"lang":"markdown","files":40}],"problems":["go: 412 file(s) but gopls is not on PATH"]}
EOF
exit 1; fi`)
	older := write("old", `echo 'Error: unknown command "doctor" for "poly-lsp-mcp"' >&2; exit 1`)

	d := &doctor{}
	d.polyLSPLanguages(newer)
	if len(d.found) != 1 {
		t.Fatalf("%+v", d.found)
	}
	f := d.found[0]
	if f.level != "warn" || f.area != "poly-lsp" || f.detail != "gopls missing for Go (412 files)" ||
		f.fix != "go install golang.org/x/tools/gopls@latest (needs go)" {
		t.Errorf("finding: %+v", f)
	}

	d = &doctor{}
	d.polyLSPLanguages(older)
	if len(d.found) != 0 {
		t.Errorf("an older build said something: %+v", d.found)
	}
}
