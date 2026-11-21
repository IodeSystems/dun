package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
)

// ragToken consults the oauth.Client (which refreshes) before falling back
// to the stored JWT file: an expired access token with a refresh token is
// transparently renewed, not returned stale.
func TestRagToken_RefreshesViaOauthClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	fresh := "fresh-access-token"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fresh,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	// Write an expired token with a refresh to the oauth client's file
	// ($DUN_HOME/oauth/raglit.json — the path oauth.NewClient("raglit",…) uses).
	oauthDir := filepath.Join(home, "oauth")
	if err := os.MkdirAll(oauthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := map[string]any{
		"access_token":   "expired-token",
		"token_type":     "Bearer",
		"refresh_token":  "refresh-abc",
		"expiry":         time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		"client_id":      "",
		"issuer":         "",
		"token_endpoint": ts.URL,
		"auth_method":    "none",
	}
	b, _ := json.Marshal(tok)
	if err := os.WriteFile(filepath.Join(oauthDir, "raglit.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// No JWT file: the old file-only behaviour would return "".
	// The fix: ragToken goes through the oauth.Client, which refreshes.
	if got := ragToken(); got != fresh {
		t.Errorf("ragToken() = %q, want %q (the refreshed token)", got, fresh)
	}
}

// When the oauth client has no token, ragToken falls back to the stored JWT file.
func TestRagToken_FallsBackToFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	// No oauth client token file (empty $DUN_HOME/oauth).
	// A JWT file exists.
	if err := harness.SaveRaglitJWT("file-jwt"); err != nil {
		t.Fatal(err)
	}

	if got := ragToken(); got != "file-jwt" {
		t.Errorf("ragToken() = %q, want %q", got, "file-jwt")
	}
}

// When the oauth client has a valid (non-expired) token, it is returned
// without hitting the file.
func TestRagToken_PrefersOauthClientOverFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	// A valid oauth client token.
	oauthDir := filepath.Join(home, "oauth")
	if err := os.MkdirAll(oauthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := map[string]any{
		"access_token": "client-token",
		"token_type":   "Bearer",
		"expiry":       time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		"client_id":    "",
		"issuer":       "",
		"auth_method":  "none",
	}
	b, _ := json.Marshal(tok)
	if err := os.WriteFile(filepath.Join(oauthDir, "raglit.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// A different JWT in the file — must NOT be returned.
	if err := harness.SaveRaglitJWT("file-jwt-should-not-appear"); err != nil {
		t.Fatal(err)
	}

	if got := ragToken(); got != "client-token" {
		t.Errorf("ragToken() = %q, want %q (the oauth client's token, not the file's)", got, "client-token")
	}
}
