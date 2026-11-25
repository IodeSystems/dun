package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/project"
)

// A user turns one tool off with "disable" on its server in dun.json:
// {"servers":[{"id":"shell","disable":["eval"]}]} hides mcpshell's eval from
// the model and leaves the server's other tools.
func TestFilterTools_ConfigDisablesShellEval(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, project.LocalServersFile), []byte(`{"servers":[{"id":"shell","disable":["eval"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := mcpserver.LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	tools := []mcpmgr.MCPTool{
		{Name: "eval", ServerID: mcpserver.ServerShell},
		{Name: "other", ServerID: mcpserver.ServerShell},
		{Name: "search", ServerID: mcpserver.ServerDocs},
	}
	got := filterTools(tools, specs)
	for _, g := range got {
		if g.Name == "eval" {
			t.Fatalf("eval still offered: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d tools, want 2: %v", len(got), got)
	}
}
