package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
)

// site/oauth/client.json is dun's OAuth client metadata document: an
// authorization server that supports them takes its URL as the client_id and
// fetches it. It must name itself and the redirect URI dun listens on.
func TestClientMetadataDocument(t *testing.T) {
	b, err := os.ReadFile("site/oauth/client.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.ClientID != "https://dun.iodesystems.com/oauth/client.json" {
		t.Errorf("client_id %q is not the document's own URL", d.ClientID)
	}
	want := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", DefaultOAuthRedirectPort)
	if !slices.Contains(d.RedirectURIs, want) {
		t.Errorf("redirect_uris %v lacks %s", d.RedirectURIs, want)
	}
	if d.AuthMethod != "none" {
		t.Errorf("token_endpoint_auth_method %q: dun is a public client", d.AuthMethod)
	}
}
