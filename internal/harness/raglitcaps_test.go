package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// RaglitCaps reads the raglit binary's --help the way IndexProtocol reads
// poly-lsp-mcp's: one run, a substring, cached by path|size|mtime. A binary
// whose --help carries the default-set marker reports DefaultSet; one
// without it, and one that cannot be found, report nothing — the gate stays
// closed and the selector is built as before.
func TestRaglitCaps_ReadsHelp(t *testing.T) {
	dir := t.TempDir()

	withMarker := filepath.Join(dir, "raglit-with")
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "Usage: raglit [flags]"
  echo "  --index default-set   read the default set"
fi
`
	if err := os.WriteFile(withMarker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	withoutMarker := filepath.Join(dir, "raglit-without")
	script2 := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "Usage: raglit [flags]"
  echo "  --index string   the index to use"
fi
`
	if err := os.WriteFile(withoutMarker, []byte(script2), 0o755); err != nil {
		t.Fatal(err)
	}

	sup, resolved, err := RaglitCaps(withMarker)
	if err != nil {
		t.Fatal(err)
	}
	if !sup.DefaultSet {
		t.Error("marker in --help: DefaultSet is false")
	}
	if resolved != withMarker {
		t.Errorf("resolved: %q, want %q", resolved, withMarker)
	}

	sup, _, err = RaglitCaps(withoutMarker)
	if err != nil {
		t.Fatal(err)
	}
	if sup.DefaultSet {
		t.Error("no marker in --help: DefaultSet is true")
	}

	// A binary that cannot be found: the gate stays closed.
	sup, _, err = RaglitCaps(filepath.Join(dir, "no-such-raglit"))
	if err == nil {
		t.Error("missing binary: expected an error")
	}
	if sup.DefaultSet {
		t.Error("missing binary: DefaultSet is true")
	}
}
