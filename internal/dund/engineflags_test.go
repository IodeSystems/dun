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
