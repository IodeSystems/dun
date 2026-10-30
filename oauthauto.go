package dun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/mcpmgr/mcpauth"
)

// The client dun logs in as when an oauth block names no client_id (USER
// 2026-10-29: "auto-register dun, if that doesn't work, we ask for
// clientId"), in this order:
//
//  1. a client ID metadata document: the authorization server says
//     client_id_metadata_document_supported, and dun's own URL is the client
//     id (site/oauth/client.json; ElevenLabs). Its redirect URI is the
//     default port's, so only with that port.
//  2. dynamic client registration (RFC 7591): the server lists a
//     registration_endpoint; dun registers a public client once and keeps it
//     in $DUN_HOME/oauth/<id>.client.json, beside the token.
//  3. neither: the error says to register a client and set client_id.

// DunClientMetadataURL is dun's client ID metadata document.
const DunClientMetadataURL = "https://dun.iodesystems.com/oauth/client.json"

// autoClient is the client dun chose: How is "metadata" or "registered".
type autoClient struct {
	ID       string `json:"client_id"`
	Secret   string `json:"client_secret,omitempty"`
	How      string `json:"how"`
	Issuer   string `json:"issuer,omitempty"`
	Redirect string `json:"redirect_uri,omitempty"`
}

// clientID is the client this login uses: the spec's, else the one dun
// chose.
func (c *oauthClient) clientID() string {
	if c.spec.ClientID != "" {
		return c.spec.ClientID
	}
	return c.auto.ID
}

func autoClientFile(id string) string { return filepath.Join(OAuthDir(), id+".client.json") }

// loadAutoClient is the client a stored token was issued to: the saved
// registration when it is that client, else dun's metadata URL.
func loadAutoClient(id, clientID string) autoClient {
	if b, err := os.ReadFile(autoClientFile(id)); err == nil {
		var a autoClient
		if json.Unmarshal(b, &a) == nil && a.ID == clientID {
			return a
		}
	}
	if clientID == DunClientMetadataURL {
		return autoClient{ID: clientID, How: "metadata"}
	}
	return autoClient{ID: clientID}
}

// chooseClient picks the client for a login to md with no client_id set.
func (c *oauthClient) chooseClient(ctx context.Context, md *mcpauth.AuthServerMetadata, redirect string, port int) (autoClient, error) {
	if md.ClientIDMetadataDocumentSupported && port == DefaultOAuthRedirectPort {
		return autoClient{ID: DunClientMetadataURL, How: "metadata", Issuer: md.Issuer}, nil
	}
	if md.RegistrationEndpoint != "" {
		if b, err := os.ReadFile(autoClientFile(c.id)); err == nil {
			var a autoClient
			if json.Unmarshal(b, &a) == nil && a.How == "registered" && a.ID != "" &&
				a.Issuer == md.Issuer && a.Redirect == redirect {
				return a, nil
			}
		}
		a, err := c.register(ctx, md, redirect)
		if err != nil {
			return a, err
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		if err := os.MkdirAll(OAuthDir(), 0o700); err != nil {
			return a, err
		}
		return a, writeFileAtomic(autoClientFile(c.id), append(b, '\n'), 0o600)
	}
	why := "offers neither dynamic client registration nor client ID metadata documents"
	if md.ClientIDMetadataDocumentSupported {
		why = fmt.Sprintf("takes dun's client URL only with the default redirect port %d (this server's redirect_port is %d)", DefaultOAuthRedirectPort, port)
	}
	return autoClient{}, fmt.Errorf("%s: its authorization server %s %s — register a client there with redirect URI %s and set oauth.client_id (/mcp add asks for it)",
		c.id, md.Issuer, why, redirect)
}

// register is RFC 7591: a public client (PKCE, no secret) for this redirect.
func (c *oauthClient) register(ctx context.Context, md *mcpauth.AuthServerMetadata, redirect string) (autoClient, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "dun",
		"client_uri":                 "https://dun.iodesystems.com",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return autoClient{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return autoClient{}, fmt.Errorf("client registration at %s: %w", md.RegistrationEndpoint, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.Unmarshal(b, &r)
	if resp.StatusCode/100 != 2 || r.ClientID == "" {
		why := strings.TrimSpace(r.Error + " " + r.Description)
		if why == "" {
			why = strings.TrimSpace(string(b[:min(len(b), 200)]))
		}
		return autoClient{}, fmt.Errorf("client registration at %s: HTTP %d %s — set oauth.client_id instead", md.RegistrationEndpoint, resp.StatusCode, why)
	}
	return autoClient{ID: r.ClientID, Secret: r.ClientSecret, How: "registered", Issuer: md.Issuer, Redirect: redirect}, nil
}

// wantsOAuth reports whether a remote server with no credentials of its
// own answers an unauthenticated initialize with a 401 Bearer challenge
// naming its protected-resource metadata (RFC 9728): the MCP OAuth flow.
func wantsOAuth(ctx context.Context, s ServerSpec) bool {
	if s.URL == "" || s.Bearer != "" {
		return false
	}
	for k := range s.Headers {
		if strings.EqualFold(k, "Authorization") {
			return false
		}
	}
	p, err := probeMCP(ctx, s.URL)
	if err != nil || p.status != http.StatusUnauthorized {
		return false
	}
	ch, ok := mcpauth.BearerChallenge(p.www)
	return ok && ch.Params["resource_metadata"] != ""
}
