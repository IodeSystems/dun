package main

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// --rag=false (force off for this run) must be distinguishable from not passing
// --rag at all (use whatever /rag auto saved). A plain bool cannot say that.
func TestTristate_UnsetIsNotFalse(t *testing.T) {
	var rag, lsp tristate
	if got := autostartOverrides(rag, lsp); got != nil {
		t.Fatalf("nothing passed should override nothing, got %v", got)
	}
	if err := rag.Set("false"); err != nil {
		t.Fatal(err)
	}
	got := autostartOverrides(rag, lsp)
	if v, ok := got[dun.ServerDocs]; !ok || v {
		t.Fatalf("--rag=false should force docs off, got %v", got)
	}
	if _, ok := got[dun.ServerCode]; ok {
		t.Error("an unset --lsp must not be overridden")
	}
	if rag.String() != "false" {
		t.Errorf("String() must round-trip for procArgs: %q", rag.String())
	}
}

// The hint is the only thing that tells a user a tool family is missing —
// the alternative symptom is an agent that silently never searches.
func TestServerHint_NamesWhatIsOffAndHow(t *testing.T) {
	hint := serverHint([]dun.ServerState{
		{ID: dun.ServerShell, Running: true},
		{ID: dun.ServerDocs},
		{ID: dun.ServerCode, Err: "mcp: initialize code: transport closed\nsome-lsp: bad flag"},
	})
	if strings.Contains(hint, dun.ServerShell) {
		t.Error("a running server should not be in the hint")
	}
	if !strings.Contains(hint, "/rag on") || !strings.Contains(hint, "/rag auto") {
		t.Errorf("docs hint should name both commands: %q", hint)
	}
	if !strings.Contains(hint, "bad flag") {
		t.Errorf("a failed start must say why: %q", hint)
	}
	if strings.Contains(hint, "\ncode did not start") && strings.Count(hint, "\n") > 1 {
		t.Errorf("failure detail should be flattened to one line: %q", hint)
	}
}
