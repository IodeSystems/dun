package dund

import "testing"

// --no-tools is an engine flag a client may pass; --no-rag and --no-lsp reach
// dund as --rag=false and --lsp=false.
func TestCheckArgs_NoToolsRagLsp(t *testing.T) {
	if err := checkArgs([]string{"--no-tools", "--rag=false", "--lsp=false"}); err != nil {
		t.Error(err)
	}
	if err := checkArgs([]string{"--media", "x.png"}); err == nil {
		t.Error("--media is a one-shot flag, not an engine flag")
	}
}

// --settle-delay=-1s turns settle off for a worker (dun mcp's default).
func TestCheckArgs_SettleDelay(t *testing.T) {
	for _, a := range [][]string{{"--settle-delay=-1s"}, {"--settle-delay", "-1s"}} {
		if err := checkArgs(a); err != nil {
			t.Error(a, err)
		}
	}
}

func TestCheckArgs_SettleDelayValidated(t *testing.T) {
	for _, a := range [][]string{{"--settle-delay=0"}, {"--settle-delay=30s"}, {"--settle-delay", "5m"}, {"--settle-delay=-1s"}} {
		if err := checkArgs(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
	for _, a := range [][]string{{"--settle-delay=1s"}, {"--settle-delay=29s"}, {"--settle-delay=soon"}, {"--settle-delay=-1"},
		{"--settle-delay", "--no-tools"}, {"--settle-delay"}, {"--model", "--url=x"}} {
		if err := checkArgs(a); err == nil {
			t.Errorf("%v must be refused", a)
		}
	}
}
