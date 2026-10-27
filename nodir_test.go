package dun

import "testing"

// A --no-dir session keeps the shell and servers that need no directory,
// and leaves out poly-lsp, raglit and any server that names the workspace.
func TestNoDir_ServersThatNeedADirectoryAreLeftOut(t *testing.T) {
	ws := "/home/u/.dun/nodir"
	servers := []Server{
		{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", ws}},
		{ID: ServerCode, Command: "poly-lsp-mcp"},
		{ID: ServerDocs, Command: "raglit"},
		{ID: "playwright", Command: "npx", Args: []string{"@playwright/mcp@latest"}},
		{ID: "repo-notes", Command: "notes", Args: []string{"--root", ws}},
		{ID: "remote", URL: "https://mcp.example/v1"},
	}
	got := withoutDirServers(servers, Config{NoDir: true, Workspace: ws})
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids[ServerShell] || !ids["playwright"] || !ids["remote"] || ids[ServerCode] || ids[ServerDocs] || ids["repo-notes"] {
		t.Errorf("kept %v", ids)
	}
	if n := len(withoutDirServers(servers, Config{Workspace: ws})); n != len(servers) {
		t.Errorf("a session with a directory lost servers: %d", n)
	}
}
