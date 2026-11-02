package tui

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/enginecmd"
)

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
		s := plainInfo(enginecmd.McpInfoIn{Servers: srv, Parts: []dun.ContextPart{{Name: "mcp: poly-lsp", Tokens: 300}}, Counted: true})
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
