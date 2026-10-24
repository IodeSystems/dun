package dun

import (
	"bufio"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// fakeRagCaps is raglit's capability list (cmd/raglit/doctorreport.go) with
// embed and vision as mode says: "off" (no models, no tesseract: semantic
// and scans off), "nosemantic" (tesseract reads scans), "on" (both set).
func fakeRagCaps(mode string) []map[string]any {
	semantic := map[string]any{"name": CapSemantic, "on": false,
		"needs": "an embedding model (embed role)", "how": ragSetRoles}
	scans := map[string]any{"name": CapScans, "on": false,
		"needs": "a vision model (vision role) or tesseract", "how": ragSetRoles + "; or sudo apt-get install tesseract-ocr"}
	switch mode {
	case "nosemantic":
		scans = map[string]any{"name": CapScans, "on": true, "via": "tesseract (no model: plain OCR, no figure descriptions)"}
	case "on":
		semantic = map[string]any{"name": CapSemantic, "on": true, "via": "text-embedding-3-small"}
		scans = map[string]any{"name": CapScans, "on": true, "via": "qwen-vl"}
	}
	return []map[string]any{
		{"name": "keyword search over text, markdown and code", "on": true, "via": "built in (SQLite FTS5)"},
		semantic,
		{"name": "PDF text", "on": false, "needs": "poppler (pdftotext, pdftoppm)", "how": "sudo apt-get install poppler-utils"},
		scans,
		{"name": "audio and video transcripts", "on": false, "needs": "a transcription model (audio role)", "how": ragSetRoles},
	}
}

func fakeRagData(mode string) map[string]any {
	embed := map[string]any{"role": "embed", "set": false, "hasKey": false, "adds": "semantic and hybrid search, similar documents"}
	if mode == "on" {
		embed = map[string]any{"role": "embed", "provider": "openrouter", "model": "text-embedding-3-small", "set": true, "hasKey": true,
			"adds": "semantic and hybrid search, similar documents"}
	}
	return map[string]any{
		"capabilities": fakeRagCaps(mode),
		"roles": []map[string]any{embed,
			{"role": "segment", "set": false, "hasKey": false, "fallsBackTo": "vision", "adds": "splits transcribed pages into passages"},
			{"role": "identity", "set": false, "hasKey": false, "fallsBackTo": "segment", "adds": "a caption, summary and kind per document"},
		},
		"extractors": []map[string]any{
			{"name": "pdftotext", "found": false, "adds": "PDF text", "optional": false,
				"install": map[string]any{"cmd": "sudo apt-get install poppler-utils", "needs": []string{"system"}}},
			{"name": "pandoc", "found": false, "adds": "Word, EPUB and ODT text", "optional": true},
			{"name": "tesseract", "found": mode != "off", "path": "/usr/bin/tesseract", "adds": "OCR without a model", "optional": true},
		},
		"extractorsSeenBy": "daemon (pid 42)",
		"missing":          3,
	}
}

// fakeRagSetup sends raglit's setup notification for mode.
func fakeRagSetup(out *bufio.Writer, mode string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message",
		"params": map[string]any{"level": "warning", "logger": "raglit/setup", "data": fakeRagData(mode)}})
	out.Write(append(b, '\n'))
	out.Flush()
}

// providerless stands in for a logged-out dun's model (cmd/dun noProvider).
type providerless struct{ scriptRunner }

func (*providerless) Providerless() bool { return true }

func startRagSetup(t *testing.T, mode string, client agent.LLMRunner) (*Harness, chan string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	rag, _ := fakeBin(t, "raglit", "raglit", fakeSetupEnv+"="+mode)
	lines := make(chan string, 8)
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: client, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers:       []Server{{ID: ServerDocs, Command: rag, Args: []string{"serve"}, Timeout: 20, Autostart: true}},
		OnServerSetup: func(id, msg string) { lines <- msg },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, lines
}

func docsSetup(t *testing.T, h *Harness) *SetupReport {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, st := range h.Servers() {
			if st.ID == ServerDocs && st.Setup != nil {
				return st.Setup
			}
		}
	}
	t.Fatal("no setup report")
	return nil
}

// Semantic search off, with a provider to pick from: one quiet line, once
// per session; the report is kept for /raglit and /mcp info.
func TestRagSetup_SemanticOffIsSaidOnce(t *testing.T) {
	h, lines := startRagSetup(t, "nosemantic", &scriptRunner{})
	select {
	case l := <-lines:
		if want := "raglit: semantic search is off — /raglit setup picks an embedding model from your providers"; l != want {
			t.Errorf("line:\n got %q\nwant %q", l, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nothing said")
	}
	r := docsSetup(t, h)
	if !r.CapabilityOff(CapSemantic) || r.CapabilityOff(CapScans) {
		t.Errorf("report: %+v", r.Capabilities)
	}
	if again := h.noteSetup(ServerDocs, *r); again != "" {
		t.Errorf("said twice: %q", again)
	}
}

// Both core model capabilities off: one line names both.
func TestRagSetup_LineNamesBoth(t *testing.T) {
	var r SetupReport
	b, _ := json.Marshal(fakeRagData("off"))
	_ = json.Unmarshal(b, &r)
	want := "raglit: semantic search and scanned pages and photos are off — /raglit setup picks the models from your providers"
	if got := SetupLine(r, "raglit"); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// What the conversation never hears: a report with the core model
// capabilities on (PDF text, audio and optional programs off), and any
// report when there is no provider to pick from.
func TestRagSetup_QuietCases(t *testing.T) {
	h, lines := startRagSetup(t, "on", &scriptRunner{})
	docsSetup(t, h)
	noLine(t, lines)

	h, lines = startRagSetup(t, "off", &providerless{})
	if r := docsSetup(t, h); !r.CapabilityOff(CapSemantic) {
		t.Fatal("the report is kept all the same")
	}
	noLine(t, lines)
}

// The /raglit table: every capability on or off (raglit's own "how" for a
// role is dun's command), the roles with their model or fallback, and the
// programs found or missing with the install command.
func TestRagSetup_Table(t *testing.T) {
	var r SetupReport
	b, _ := json.Marshal(fakeRagData("nosemantic"))
	_ = json.Unmarshal(b, &r)
	got := CapabilityTable(r, "raglit")
	for _, want := range []string{
		"  on   keyword search over text, markdown and code · built in (SQLite FTS5)",
		"  off  semantic and hybrid search · needs an embedding model (embed role) · /raglit setup",
		"  off  PDF text · needs poppler (pdftotext, pdftoppm) · sudo apt-get install poppler-utils",
		"  on   scanned pages and photos · tesseract (no model: plain OCR, no figure descriptions)",
		"models, by role:",
		"  embed      none · semantic and hybrid search, similar documents",
		"  segment    none → vision's model · splits transcribed pages into passages",
		"  summaries  none → segment's model",
		"programs it reads files with (as the daemon (pid 42) sees them):",
		"  pdftotext missing · adds PDF text · sudo apt-get install poppler-utils",
		"  pandoc missing (optional) · adds Word, EPUB and ODT text",
		"  tesseract found",
		"/raglit setup picks a model for each role",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PUT /api") {
		t.Errorf("raglit's own how leaked:\n%s", got)
	}
	var on SetupReport
	b, _ = json.Marshal(fakeRagData("on"))
	_ = json.Unmarshal(b, &on)
	if got := CapabilityTable(on, "raglit"); !strings.Contains(got, "  embed      openrouter text-embedding-3-small · semantic") {
		t.Errorf("a set role:\n%s", got)
	}
}

func TestParseRaglitDoctor(t *testing.T) {
	d := fakeRagData("off")
	d["home"], d["daemon"], d["update"] = "/h", "http://127.0.0.1:1", map[string]any{"origin": "source", "current": "abc"}
	b, _ := json.Marshal(d)
	got, ok := ParseRaglitDoctor(b)
	if !ok || got.Home != "/h" || got.Daemon != "http://127.0.0.1:1" || len(got.Roles) != 3 || !got.CapabilityOff(CapSemantic) || len(got.Update) == 0 {
		t.Errorf("%v %+v", ok, got)
	}
	for _, bad := range []string{"raglit doctor — OCR readiness", `{"languages":[]}`} {
		if _, ok := ParseRaglitDoctor([]byte(bad)); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
