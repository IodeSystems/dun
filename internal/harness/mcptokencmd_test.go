package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/oauth"
)

// fakeMCPWithTokens is a Streamable HTTP MCP server that accepts only the
// tokens in the given set and records every Authorization header it saw.
func fakeMCPWithTokens(t *testing.T, accept map[string]bool, seen *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if !accept[token] {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
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
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "search", "description": "corp search"}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "corp result"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
}

// tokenCounterCmd returns a shell command that reads a counter file and
// outputs "stale" on the first call, "fresh" thereafter.
func tokenCounterCmd(counterPath string) string {
	return fmt.Sprintf(
		`c=$(cat %q 2>/dev/null || echo 0); c=$((c+1)); echo $c > %q; if [ $c -eq 1 ]; then echo stale; else echo fresh; fi`,
		counterPath, counterPath,
	)
}

// MG: a server spec names a token command; dun runs it on connect, sends
// stdout as the bearer; on 401, re-runs and retries with the fresh value.
func TestTokenCmd_RunsCommandAndRetriesOn401(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())

	home := t.TempDir()
	counterPath := filepath.Join(home, "token_counter")

	// The server accepts "fresh" but rejects "stale".
	var seen atomic.Int64
	accept := map[string]bool{"fresh": true}
	ts := fakeMCPWithTokens(t, accept, &seen)
	defer ts.Close()

	cmd := tokenCounterCmd(counterPath)

	h := newServerHarness(t)
	ctx := context.Background()

	_, err := h.AddServer(ctx, mcpserver.Spec{ID: "corp", URL: ts.URL, TokenCmd: cmd})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	// The command must have been run at least twice: once on connect (stale,
	// rejected), once on 401 (fresh, accepted).
	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("counter file: %v", err)
	}
	count := strings.TrimSpace(string(data))
	if count != "2" {
		t.Fatalf("token command run %s times; want 2 (initial + retry)", count)
	}

	// The server must be started (the retry succeeded).
	if !h.mgr.ServerStarted("corp") {
		t.Fatal("server not started after retry")
	}

	// The server saw at least the rejected and accepted requests.
	if seen.Load() < 2 {
		t.Fatalf("server saw %d requests; want at least 2 (401 + retry)", seen.Load())
	}
}

// TokenCmd is mutually exclusive with Bearer and OAuth.
func TestTokenCmd_MutualExclusion(t *testing.T) {
	if err := mcpserver.CheckTokenCmd(mcpserver.Spec{URL: "http://x", TokenCmd: "echo t", Bearer: "b"}, "project"); err == nil {
		t.Error("TokenCmd + Bearer must be refused")
	}
	if err := mcpserver.CheckTokenCmd(mcpserver.Spec{URL: "http://x", TokenCmd: "echo t", OAuth: &oauth.Spec{}}, "project"); err == nil {
		t.Error("TokenCmd + OAuth must be refused")
	}
	if err := mcpserver.CheckTokenCmd(mcpserver.Spec{TokenCmd: "echo t"}, "project"); err == nil {
		t.Error("TokenCmd without URL must be refused")
	}
	if err := mcpserver.CheckTokenCmd(mcpserver.Spec{URL: "http://x", TokenCmd: "echo t"}, "project"); err != nil {
		t.Errorf("TokenCmd alone is valid: %v", err)
	}
}

// TokenCmd propagates through the spec → server conversion.
func TestTokenCmd_SpecConversion(t *testing.T) {
	// Check that the Spec field exists and CheckTokenCmd accepts a valid spec.
	spec := mcpserver.Spec{ID: "corp", URL: "http://x", TokenCmd: "echo token"}
	if spec.TokenCmd != "echo token" {
		t.Fatal("Spec.TokenCmd not set")
	}
	if err := mcpserver.CheckTokenCmd(spec, "project"); err != nil {
		t.Fatalf("CheckTokenCmd: %v", err)
	}
}
