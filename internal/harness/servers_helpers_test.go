package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// The config-file test helpers, as internal/mcpserver's tests have them.

func writeJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func byID(servers []mcpserver.Server) map[string]mcpserver.Server {
	m := map[string]mcpserver.Server{}
	for _, s := range servers {
		m[s.ID] = s
	}
	return m
}

// setAutostart writes dir's project switch for id, as /mcp on --project does.
func setAutostart(dir, id string, on bool) (string, error) {
	return mcpserver.UpdateLocalServer(dir, id, func(s *mcpserver.Spec) { s.Autostart = &on })
}
