package dun

// What dun does with raglit and poly-lsp-mcp, pinned end to end through the
// harness: the server is started by its id, and what dun asks of it (the CLI
// runs, the tool calls, the env it is started with) is read back from the fake
// (fakebin_test.go). These pass unchanged however that wiring is expressed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// startFakeRaglit starts a harness on ws whose raglit is the fake, with the
// serve args dun gives the real one, plus any other servers. It returns the
// harness, the fake's log and the hits file its search answers from.
func startFakeRaglit(t *testing.T, ws string, start bool, others ...Server) (*Harness, string, string) {
	t.Helper()
	hits := filepath.Join(t.TempDir(), "hits.json")
	cmd, logPath := fakeBin(t, "raglit", "raglit", fakeHitsEnv+"="+hits)
	servers := append([]Server{{ID: ServerDocs, Command: cmd, Timeout: 20,
		Args: []string{"serve", "--project", raglitProject(ws), "--index", raglitIndex(ws)}}}, others...)
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if start {
		if err := h.StartServer(context.Background(), ServerDocs); err != nil {
			t.Fatal(err)
		}
	}
	return h, logPath, hits
}

// logHas waits for a line of the fake's log that starts with prefix and
// contains every one of parts, and returns it.
func logHas(t *testing.T, logPath, prefix string, parts ...string) string {
	t.Helper()
	var found string
	deadline := time.Now().Add(10 * time.Second)
	for found == "" {
		for _, l := range fakeLog(logPath) {
			if strings.HasPrefix(l, prefix) && containsAll(l, parts) {
				found = l
			}
		}
		if found == "" {
			if time.Now().After(deadline) {
				t.Fatalf("no %q line with %q in the log:\n%s", prefix, parts, strings.Join(fakeLog(logPath), "\n"))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return found
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func writeHits(t *testing.T, path string, ids ...string) {
	t.Helper()
	var hits []map[string]any
	for _, id := range ids {
		hits = append(hits, map[string]any{"doc_id": id, "title": filepath.Base(id), "score": 1.0, "snippet": "a fragment"})
	}
	b, _ := json.Marshal(hits)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// surfaceOnce appends a user message and runs the preparer the turn would,
// returning the aside it appended ("" for none).
func surfaceOnce(t *testing.T, h *Harness, text string) string {
	t.Helper()
	if h.docsPrep == nil {
		t.Fatal("no proactive search is armed")
	}
	ctx := context.Background()
	before := len(h.store.docsEntries())
	if err := h.store.Append(ctx, h.Session.SessionID, agent.Entry{Kind: agent.KindUser, Content: text, CreatedAt: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if err := h.Session.Preparer.PrepareNotifications(ctx, h.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	es := h.store.docsEntries()
	if len(es) == before {
		return ""
	}
	return es[len(es)-1]
}

func callTool(t *testing.T, h *Harness, name string, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = name, string(b)
	out, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func modelToolNames(h *Harness) []string {
	var out []string
	for _, td := range h.Session.Tools {
		out = append(out, td.Function.Name)
	}
	return out
}

// Starting raglit indexes the workspace into the serve command's project and
// index, and the summary is the server's to show.
func TestRaglitStart_IngestsTheWorkspace(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "README.md"), "the zebra cluster")
	h, logPath, _ := startFakeRaglit(t, ws, true)
	logHas(t, logPath, "raglit ingest --project "+raglitProject(ws)+" --index "+raglitIndex(ws)+" --now", "README.md")
	st, _ := serverState(h, ServerDocs)
	if !st.Running || st.Ingest == "" {
		t.Errorf("raglit should be up and carry its ingest summary: %+v", st)
	}
	if st, _ := serverState(h, ServerDocs); st.ID != ServerDocs {
		t.Fatal("raglit is not listed")
	}
}

// Before a turn, raglit's search is asked about the conversation and each
// group's clear winner comes back as one aside: a memory note (with the
// refine cue), a skill (as a load cue naming the skill), an MCP server's
// documentation, and the workspace's own file (inlined, being short).
func TestRaglit_SurfacesMemorySkillMCPAndWorkspaceHitsBeforeATurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	writeSkill(t, filepath.Join(home, "skills"), "zebra-deploy", "---\nname: zebra-deploy\ndescription: deploy to the zebra cluster\n---\nuse --frostbite")
	ws := t.TempDir()
	h, logPath, hits := startFakeRaglit(t, ws, true)
	skillMD := filepath.Join(home, "skills", "zebra-deploy", "SKILL.md")
	waitFor(t, 10*time.Second, func() bool { _, ok := h.skillForDoc(skillMD); return ok })

	m := h.memoryStore()
	if m == nil {
		t.Fatal("no memory store while raglit runs")
	}
	note := filepath.Join(m.dir, "zebra.md")
	mcpDoc := filepath.Join(mcpDocsRoot(), "corp", "runbook.md")
	wsDoc := filepath.Join(ws, "notes.md")
	writeRule(t, note, "Zebra deploys need --frostbite.")
	writeRule(t, mcpDoc, "# Runbook\nzebra nodes run the old kernel")
	writeRule(t, wsDoc, "zebra notes from the workspace")
	writeHits(t, hits, note, skillMD, mcpDoc, wsDoc)

	aside := surfaceOnce(t, h, "how do I deploy to zebra?")
	for _, want := range []string{
		"A note an earlier session on this project saved may apply", "Zebra deploys need --frostbite.",
		"A skill matches what you are working on", `call skill name="zebra-deploy"`,
		"From the documentation of an MCP server you have:", "zebra nodes run the old kernel",
		"zebra notes from the workspace",
	} {
		if !strings.Contains(aside, want) {
			t.Errorf("the aside lacks %q:\n%s", want, aside)
		}
	}
	logHas(t, logPath, "call search", "deploy to zebra")
	if surfaceOnce(t, h, "and zebra again") != "" {
		t.Error("what surfaced once must not surface again")
	}

	if err := h.StopServer(ServerDocs); err != nil {
		t.Fatal(err)
	}
	if h.docsPrep != nil {
		t.Error("the proactive search outlived raglit")
	}
}

// The memory tool exists while raglit runs; a note it adds is a file in the
// project's memory dir, ingested into index memory of raglit's store.
func TestRaglit_MemoryToolIndexesANoteAndGoesWithTheServer(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, logPath, _ := startFakeRaglit(t, ws, false)
	if slices.Contains(modelToolNames(h), "memory") {
		t.Fatal("memory without raglit")
	}
	if err := h.StartServer(context.Background(), ServerDocs); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(modelToolNames(h), "memory") {
		t.Fatalf("no memory tool with raglit up: %v", modelToolNames(h))
	}
	out := callTool(t, h, "memory", map[string]any{"action": "add", "content": "The user wants commits small.", "question": "how big should commits be?"})
	if !strings.Contains(out, "saved ") {
		t.Fatalf("add: %q", out)
	}
	dir := memoryDir(raglitProject(ws))
	logHas(t, logPath, "raglit ingest --project "+raglitProject(ws)+" --index memory --now "+dir+string(os.PathSeparator))
	if err := h.StopServer(ServerDocs); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(modelToolNames(h), "memory") || h.memoryStore() != nil {
		t.Error("memory outlived raglit")
	}
}

// With raglit up, the local skills go into index skills: each SKILL.md and
// its reference files, and a hit on either maps back to the skill.
func TestRaglit_SkillsIndexTakesSkillMDAndReferenceFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	writeSkill(t, filepath.Join(home, "skills"), "release", "---\nname: release\ndescription: cut a release\n---\nsee checklist.md")
	ref := filepath.Join(home, "skills", "release", "checklist.md")
	writeRule(t, ref, "- tag it")
	ws := t.TempDir()
	h, logPath, _ := startFakeRaglit(t, ws, true)
	skillMD := filepath.Join(home, "skills", "release", "SKILL.md")
	logHas(t, logPath, "raglit ingest --project "+raglitProject(ws)+" --index skills --now", skillMD, ref)
	waitFor(t, 10*time.Second, func() bool { hit, ok := h.skillForDoc(ref); return ok && hit.File == "checklist.md" })
}

// /rules index puts the root rules' imports into index rules, and a hit on
// one surfaces under the rules cue.
func TestRaglit_RulesIndexModeIngestsTheImportsAndSurfacesThem(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root rules\n@docs/zebra.md")
	imp := filepath.Join(ws, "docs", "zebra.md")
	writeRule(t, imp, "the zebra fact: always pass --frostbite")
	h, logPath, hits := startFakeRaglit(t, ws, false)
	if _, err := h.SetRulesMode(context.Background(), RulesIndex); err == nil || !strings.Contains(err.Error(), "/raglit on") {
		t.Errorf("index mode without raglit: %v", err)
	}
	if err := h.StartServer(context.Background(), ServerDocs); err != nil {
		t.Fatal(err)
	}
	msg, err := h.SetRulesMode(context.Background(), RulesIndex)
	if err != nil || !strings.Contains(msg, `going into index "rules"`) {
		t.Fatalf("index mode: %q %v", msg, err)
	}
	logHas(t, logPath, "raglit ingest --project "+raglitProject(ws)+" --index rules --now", imp)
	waitFor(t, 10*time.Second, func() bool { return h.isRulesDoc(imp) })
	if strings.Contains(h.Session.System, "the zebra fact") {
		t.Error("an indexed import is still in the system prompt")
	}
	writeHits(t, hits, imp)
	aside := surfaceOnce(t, h, "what about zebra?")
	if !strings.Contains(aside, "From a file the project's rules import") || !strings.Contains(aside, "the zebra fact") {
		t.Errorf("the rules hit: %q", aside)
	}
	if !strings.Contains(h.RulesStatus(), "mode index") {
		t.Errorf("status: %q", h.RulesStatus())
	}
}

// fakeDocsMCP is a Streamable HTTP MCP server with one tool and two text
// resources.
func fakeDocsMCP(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				URI string `json:"uri"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "lookup", "description": "look up"}}}
		case "resources/list":
			result = map[string]any{"resources": []map[string]any{
				{"uri": "docs://runbook", "name": "Runbook", "mimeType": "text/markdown"},
				{"uri": "docs://api", "name": "API", "mimeType": "text/markdown"}}}
		case "resources/read":
			result = map[string]any{"contents": []map[string]any{{"uri": msg.Params.URI, "mimeType": "text/markdown", "text": "text of " + msg.Params.URI}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// /mcp refresh (and /mcp add after it starts the server) copies a server's
// text resources to files and ingests them into index mcp-<id> of raglit's
// store — through the raglit CLI, so raglit need not be running.
func TestRaglit_MCPDocsRefreshIngestsIntoTheServersIndex(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	ts := fakeDocsMCP(t)
	h, logPath, _ := startFakeRaglit(t, ws, false, Server{ID: "corp", URL: ts.URL, Timeout: 10})
	if err := h.StartServer(context.Background(), "corp"); err != nil {
		t.Fatal(err)
	}
	msg, err := h.SyncMCPDocs(context.Background(), "corp")
	if err != nil || !strings.Contains(msg, "2 indexed") || !strings.Contains(msg, "index mcp-corp") {
		t.Fatalf("sync: %q %v", msg, err)
	}
	logHas(t, logPath, "raglit ingest --project "+raglitProject(ws)+" --index mcp-corp --now", mcpDocsDir("corp"))
}

// Starting poly-lsp-mcp arms the AGENTS.md guard: POLY_LSP_HOOK names the
// embedded script and DUN_AGENTS_STATE the workspace's state file. No other
// server gets them.
func TestPolyLspStart_ArmsTheAgentsMDGuard(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, lspLog := fakeBin(t, "poly-lsp-mcp", "poly-lsp")
	other, otherLog := fakeBin(t, "poly-lsp-mcp", "poly-lsp")
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []Server{
		{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20},
		{ID: "other", Command: other, Timeout: 20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, id := range []string{ServerCode, "other"} {
		if err := h.StartServer(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	line := logHas(t, lspLog, "start POLY_LSP_HOOK=/", " DUN_AGENTS_STATE="+agentsStateFile(ws))
	hook := strings.TrimPrefix(strings.Fields(line)[1], "POLY_LSP_HOOK=")
	b, err := os.ReadFile(hook)
	if err != nil || string(b) != agentsHookScript {
		t.Errorf("POLY_LSP_HOOK is not the guard script: %v", err)
	}
	if st, err := os.Stat(hook); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("the hook is not executable: %v", err)
	}
	logHas(t, otherLog, "start POLY_LSP_HOOK= DUN_AGENTS_STATE=")
}
