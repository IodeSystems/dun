package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Install hints (dun/setupstate.go) in /mcp info and in doctor.

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
