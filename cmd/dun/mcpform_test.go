package main

import (
	"strings"
	"testing"
)

func TestMCPFormSpec_URLOrCommand(t *testing.T) {
	s := mcpFormSpec(" corp ", "https://corp/mcp", " tok ")
	if s.ID != "corp" || s.URL != "https://corp/mcp" || s.Bearer != "tok" || s.Command != "" {
		t.Errorf("remote: %+v", s)
	}
	s = mcpFormSpec("db", "db-mcp --stdio --root /x", "tok")
	if s.Command != "db-mcp" || len(s.Args) != 3 || s.URL != "" || s.Bearer != "" {
		t.Errorf("a command line is a stdio server and takes no bearer: %+v", s)
	}
	for _, bad := range []string{"", "Corp", "-x", "a b", "a.b"} {
		if mcpIDOK(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The token never reaches the screen or the slash history.
func TestMCPForm_MasksTheToken(t *testing.T) {
	m := newTUIModel(nil, ".")
	m.openMCPForm()
	m.mcpStep = 2
	m.mcpFields[2].SetValue("sk-very-secret")
	if v := stripANSI(m.mcpFormPanel()); strings.Contains(v, "sk-very-secret") {
		t.Errorf("token shown:\n%s", v)
	}
}
