package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/internal/update"
)

// ragDoctorJSON is a `raglit doctor --json`: semantic search off (on when
// embed is set), the five roles, two programs, and an update block.
func ragDoctorJSON(daemon, embed string) string {
	semantic := `{"name":"semantic and hybrid search","on":false,"needs":"an embedding model (embed role)","how":"raglit init, or PUT /api/settings/models on the daemon"}`
	role := `{"role":"embed","set":false,"hasKey":false,"adds":"semantic and hybrid search, similar documents"}`
	if embed != "" {
		semantic = `{"name":"semantic and hybrid search","on":true,"via":"` + embed + `"}`
		role = `{"role":"embed","provider":"openrouter","model":"` + embed + `","set":true,"hasKey":true,"adds":"semantic and hybrid search, similar documents"}`
	}
	return `{"binary":"/x/raglit","home":"/x/.raglit","configured":true,"daemon":"` + daemon + `",
"capabilities":[{"name":"keyword search over text, markdown and code","on":true,"via":"built in (SQLite FTS5)"},` + semantic + `,
 {"name":"PDF text","on":false,"needs":"poppler (pdftotext, pdftoppm)","how":"sudo apt-get install poppler-utils"},
 {"name":"scanned pages and photos","on":true,"via":"qwen-vl"}],
"roles":[` + role + `,
 {"role":"vision","provider":"local","model":"qwen-vl","set":true,"hasKey":false,"adds":"OCR of scans and photos, figure descriptions"},
 {"role":"segment","set":false,"hasKey":false,"fallsBackTo":"vision","adds":"splits transcribed pages into passages"},
 {"role":"identity","set":false,"hasKey":false,"fallsBackTo":"segment","adds":"a caption, summary and kind per document"},
 {"role":"audio","set":false,"hasKey":false,"adds":"audio and video transcripts"}],
"extractors":[{"name":"pdftotext","found":false,"adds":"PDF text","optional":false,"install":{"cmd":"sudo apt-get install poppler-utils","needs":["system"]}},
 {"name":"pandoc","found":false,"adds":"Word, EPUB and ODT text","optional":true}],
"extractorsSeenBy":"daemon (pid 42)",
"update":{"origin":"go install","current":"v0.1.0","latest":"v0.2.0","available":true,"how":"go install github.com/iodesystems/raglit/cmd/raglit@latest"}}`
}

// fakeRaglit puts a raglit on raglitBin that prints the file doc for
// doctor --json, and returns the file.
func fakeRaglit(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "doctor.json")
	if err := os.WriteFile(out, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "raglit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1 $2\" = \"doctor --json\" ] && exec cat '"+out+"'\necho 'raglit doctor — OCR readiness'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := tui.RaglitBin
	tui.RaglitBin = func() (string, error) { return bin, nil }
	t.Cleanup(func() { tui.RaglitBin = old })
	return out
}

// doctor relays raglit's report: capabilities off as ok lines with dun's
// command, a missing program as a warn (ok when optional) with its install
// command, and the update block; a raglit without --json adds nothing.
func TestDoctor_Raglit(t *testing.T) {
	fakeRaglit(t, ragDoctorJSON("", ""))
	d := &doctor{}
	d.raglit()
	var got []string
	for _, f := range d.found {
		got = append(got, f.level+" "+f.area+" "+f.detail+" | "+f.fix)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"ok raglit off: semantic and hybrid search — needs an embedding model (embed role) | /rag setup",
		"ok raglit off: PDF text — needs poppler (pdftotext, pdftoppm) | sudo apt-get install poppler-utils",
		"warn raglit pdftotext missing: PDF text (as the daemon (pid 42) sees it) | sudo apt-get install poppler-utils",
		"ok raglit pandoc missing (optional): Word, EPUB and ODT text (as the daemon (pid 42) sees it) | ",
		"ok raglit raglit says an update is available: v0.1.0 → v0.2.0 | go install github.com/iodesystems/raglit/cmd/raglit@latest",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %q:\n%s", want, all)
		}
	}
	fakeRaglit(t, "raglit doctor — OCR readiness")
	d = &doctor{}
	d.raglit()
	if len(d.found) != 0 {
		t.Errorf("an older raglit: %+v", d.found)
	}
	if originNote("raglit", update.Origin{Kind: update.GoInstall}) == "" || originNote("raglit", update.Origin{Kind: update.Release}) != "" {
		t.Error("the go install origin note")
	}
}
