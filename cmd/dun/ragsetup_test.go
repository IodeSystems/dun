package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/update"
)

// The key the tests' provider holds: it must never reach raglit's config.
const ragTestKey = "sk-ragtest-secret"

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
	old := raglitBin
	raglitBin = func() (string, error) { return bin, nil }
	t.Cleanup(func() { raglitBin = old })
	return out
}

// fakeDaemon is raglit's model settings API: GET answers with a local
// provider and its vision role; PUT keeps the body and, as the real daemon
// re-reads its config, makes doctor say embed is on.
type fakeDaemon struct {
	mu   sync.Mutex
	put  []byte
	srv  *httptest.Server
	docs string // the doctor file to rewrite on PUT
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	d := &fakeDaemon{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/settings/models" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"providers":[{"name":"local","kind":"custom","base_url":"http://127.0.0.1:9/v1","has_key":false}],
"roles":{"vision":{"provider":"local","model":"qwen-vl"},"embed":{"provider":"","model":""}},"presets":[],"tesseract":false}`)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			d.mu.Lock()
			d.put = b
			d.mu.Unlock()
			if d.docs != "" {
				_ = os.WriteFile(d.docs, []byte(ragDoctorJSON(d.srv.URL, "text-embedding-3-small")), 0o600)
			}
			_, _ = io.WriteString(w, `{"saved":true,"reload":"queued"}`)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// ragDunHome is a DUN_HOME whose one provider holds ragTestKey.
func ragDunHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("RAGLIT_ROOT", t.TempDir()) // never the real ~/.raglit
	cfg := `{"provider":"openrouter","providers":[{"name":"openrouter","url":"https://openrouter.example/api/v1","key":"` + ragTestKey + `"}]}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func checkKeyFile(t *testing.T, home, name string) string {
	t.Helper()
	p := filepath.Join(home, "keys", name+".key")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("no key file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != ragTestKey {
		t.Errorf("key file holds %q", b)
	}
	return p
}

// The setup end to end, against a daemon: one screen per role from
// raglit's report, the person's provider's models with /model's labels,
// keep and none; the save PUTs every provider and every role, the new
// provider's key by api_key_file (a 0600 file under $DUN_HOME), never the
// key; then what is on now, from raglit asked again.
func TestRagSetup_FlowSavesToTheDaemon(t *testing.T) {
	home := ragDunHome(t)
	d := newFakeDaemon(t)
	d.docs = fakeRaglit(t, ragDoctorJSON(d.srv.URL, ""))

	m := newTUIModel(nil, ".")
	m.w, m.h = 100, 40
	m.openRagSetup()
	if m.rag == nil || !strings.Contains(stripANSI(m.ragSetupPanel()), "asking raglit") {
		t.Fatal("the panel does not open at once")
	}
	m.ragLoaded(loadRagSetup().(ragLoadedMsg))
	m.ragModels(ragModelsMsg{modelFetchMsg{provider: "openrouter", ids: []string{"gpt-5-mini", "qwen-vl-max", "text-embedding-3-small", "whisper-1"}}})

	v := stripANSI(m.ragSetupPanel())
	for _, want := range []string{"raglit setup · embed (1 of 5)", "  semantic and hybrid search, similar documents", "none — off", "text-embedding-3-small  embeddings"} {
		if !strings.Contains(v, want) {
			t.Errorf("embed screen lacks %q:\n%s", want, v)
		}
	}
	// It starts on the embedding model: the one row this role wants.
	if c := m.rag.ragChoices()[m.rag.list.sel]; c.row.id != "text-embedding-3-small" {
		t.Errorf("embed starts on %+v", c)
	}
	keys := func(ks ...tea.KeyPressMsg) {
		for _, k := range ks {
			nm, _ := m.updateRagSetup(k)
			m = nm.(tuiModel)
		}
	}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	keys(enter) // embed: the embedding model
	if v := stripANSI(m.ragSetupPanel()); !strings.Contains(v, "summaries (2 of 5)") || !strings.Contains(v, "none — use segment's model") {
		t.Errorf("summaries screen:\n%s", v)
	}
	keys(enter) // summaries: none (as it is)
	v = stripANSI(m.ragSetupPanel())
	if !strings.Contains(v, "keep local qwen-vl") {
		t.Errorf("vision screen lacks keep:\n%s", v)
	}
	keys(enter, enter, enter) // vision keep, segment none, audio none
	v = stripANSI(m.ragSetupPanel())
	if !strings.Contains(v, "embed: none → openrouter text-embedding-3-small") || strings.Contains(v, "vision:") {
		t.Errorf("summary:\n%s", v)
	}
	nm, cmd := m.updateRagSetup(enter)
	m = nm.(tuiModel)
	if cmd == nil {
		t.Fatal("enter on the summary does not save")
	}
	m.ragSaved(cmd().(ragSavedMsg))
	if m.rag != nil {
		t.Error("the setup stays open after the save")
	}

	var body struct {
		Providers []map[string]string `json:"providers"`
		Roles     map[string]ragRole  `json:"roles"`
	}
	d.mu.Lock()
	put := d.put
	d.mu.Unlock()
	if strings.Contains(string(put), ragTestKey) {
		t.Fatalf("the key was sent to raglit: %s", put)
	}
	if err := json.Unmarshal(put, &body); err != nil {
		t.Fatalf("PUT %s: %v", put, err)
	}
	keyPath := checkKeyFile(t, home, "openrouter")
	if len(body.Providers) != 2 || body.Providers[0]["name"] != "local" ||
		body.Providers[1]["name"] != "openrouter" || body.Providers[1]["api_key_file"] != keyPath ||
		body.Providers[1]["base_url"] != "https://openrouter.example/api/v1" {
		t.Errorf("providers: %+v", body.Providers)
	}
	if body.Roles["embed"] != (ragRole{"openrouter", "text-embedding-3-small"}) || body.Roles["vision"] != (ragRole{"local", "qwen-vl"}) ||
		len(body.Roles) != 5 || body.Roles["identity"].Model != "" {
		t.Errorf("roles: %+v", body.Roles)
	}
	out := convoText(m)
	for _, want := range []string{
		"raglit: saved to the daemon at " + d.srv.URL + " (it reloads when no job is running)",
		"providers: openrouter (key read from " + keyPath + ") — raglit's config holds the file's path, never the key",
		"on now: semantic and hybrid search (text-embedding-3-small) · still off: PDF text",
		"/rag setup again, with none, to undo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("said lacks %q:\n%s", want, out)
		}
	}
}

// esc anywhere closes the setup and saves nothing.
func TestRagSetup_EscSavesNothing(t *testing.T) {
	ragDunHome(t)
	d := newFakeDaemon(t)
	fakeRaglit(t, ragDoctorJSON(d.srv.URL, ""))
	m := newTUIModel(nil, ".")
	m.openRagSetup()
	m.ragLoaded(loadRagSetup().(ragLoadedMsg))
	nm, _ := m.updateRagSetup(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	nm, _ = m.updateRagSetup(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.rag != nil || d.put != nil || !strings.Contains(convoText(m), "cancelled, nothing changed") {
		t.Errorf("open %v, put %s:\n%s", m.rag != nil, d.put, convoText(m))
	}
}

// A raglit without doctor --json: said, and the setup does not open.
func TestRagSetup_OlderRaglit(t *testing.T) {
	ragDunHome(t)
	fakeRaglit(t, "raglit doctor — OCR readiness")
	m := newTUIModel(nil, ".")
	m.openRagSetup()
	m.ragLoaded(loadRagSetup().(ragLoadedMsg))
	if m.rag != nil || !strings.Contains(convoText(m), "no doctor --json") {
		t.Errorf("%s", convoText(m))
	}
}

// No daemon answering: the setup writes the shared daemon's config.json
// ($RAGLIT_ROOT), keeping every key it does not own and a provider's inline
// key, removing a role set to none, and the new provider's key by path.
func TestRagSetup_NoDaemonWritesConfig(t *testing.T) {
	home := ragDunHome(t)
	cfgPath := filepath.Join(os.Getenv("RAGLIT_ROOT"), "config.json")
	old := `{"daemon_url":"http://x:1","ocr":{"cheap_engine":"tesseract"},
"providers":{"local":{"base_url":"http://127.0.0.1:9/v1","api_key":"inline-local"}},
"vision_provider":"local","vision_model":"qwen-vl"}`
	if err := os.WriteFile(cfgPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, err := fileSettings(cfgPath)
	if err != nil || cur.Roles["vision"] != (ragRole{"local", "qwen-vl"}) || len(cur.Providers) != 1 || !cur.Providers[0].HasKey {
		t.Fatalf("read %+v %v", cur, err)
	}
	provs := loadConfig().providerList()
	pl, err := planRagSetup(cur, map[string]ragPick{
		"embed":  {provider: "openrouter", model: "text-embedding-3-small"},
		"vision": {}, // none
		"audio":  {keep: true},
	}, provs)
	if err != nil {
		t.Fatal(err)
	}
	if err := pl.apply(ragTarget{config: cfgPath}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(b), ragTestKey) {
		t.Fatalf("the key is in raglit's config:\n%s", b)
	}
	if st, _ := os.Stat(cfgPath); st.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", st.Mode().Perm())
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	keyPath := checkKeyFile(t, home, "openrouter")
	provsGot, _ := got["providers"].(map[string]any)
	or, _ := provsGot["openrouter"].(map[string]any)
	local, _ := provsGot["local"].(map[string]any)
	switch {
	case got["daemon_url"] != "http://x:1" || got["ocr"] == nil:
		t.Errorf("keys it does not own were lost:\n%s", b)
	case local["api_key"] != "inline-local":
		t.Errorf("the local provider's inline key was lost:\n%s", b)
	case or["api_key_file"] != keyPath || or["base_url"] != "https://openrouter.example/api/v1":
		t.Errorf("new provider:\n%s", b)
	case got["embed_provider"] != "openrouter" || got["embed_model"] != "text-embedding-3-small":
		t.Errorf("embed:\n%s", b)
	case got["vision_model"] != nil || got["vision_provider"] != nil:
		t.Errorf("vision none must remove it:\n%s", b)
	}
}

// A provider raglit already has at the same address is reused, and given
// a key file only when it has no key; a name raglit has at another address
// is numbered.
func TestRagSetup_ProviderReuseAndNames(t *testing.T) {
	home := ragDunHome(t)
	cur := ragSettings{Providers: []ragProvider{
		{Name: "openrouter", BaseURL: "http://elsewhere/v1", HasKey: true},
		{Name: "or-same", BaseURL: "https://openrouter.example/api/v1/", HasKey: true},
	}}
	pl, err := planRagSetup(cur, map[string]ragPick{"embed": {provider: "openrouter", model: "e"}}, loadConfig().providerList())
	if err != nil {
		t.Fatal(err)
	}
	if pl.roles["embed"].Provider != "or-same" || len(pl.providers) != 2 || pl.providers[1].KeyFile != "" {
		t.Errorf("reuse: %+v %+v", pl.roles, pl.providers)
	}
	if _, err := os.Stat(filepath.Join(home, "keys")); err == nil {
		t.Error("a key file was written for a provider that has a key")
	}
	cur.Providers = cur.Providers[:1]
	pl, _ = planRagSetup(cur, map[string]ragPick{"embed": {provider: "openrouter", model: "e"}}, loadConfig().providerList())
	if pl.roles["embed"].Provider != "openrouter-2" {
		t.Errorf("numbered: %+v", pl.roles)
	}
	if got := ragProviderName("My_Gateway (corp)"); got != "my-gateway-corp" {
		t.Errorf("ragProviderName = %q", got)
	}
}

// A daemon with auth on refuses: said in words, not saved.
func TestRagSetup_DaemonRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"detail":"admin only"}`)
	}))
	defer srv.Close()
	err := ragPlan{roles: map[string]ragRole{}}.apply(ragTarget{daemon: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "admin only (HTTP 403) — set them in its web UI") {
		t.Errorf("%v", err)
	}
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

// /mcp info's raglit row says semantic search is off.
func TestMCPInfo_SemanticOff(t *testing.T) {
	var rep dun.SetupReport
	_ = json.Unmarshal([]byte(ragDoctorJSON("", "")), &rep)
	st := []dun.ServerState{{ID: "raglit", Slash: "raglit", Running: true, Auto: true, Tools: 4, Setup: &rep}}
	if s := plainInfo(mcpInfoIn{servers: st}); !strings.Contains(s, "  raglit  running · built-in · 4 tools · semantic search off\n") {
		t.Errorf("%s", s)
	}
}
