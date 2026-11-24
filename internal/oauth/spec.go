package oauth

// Spec is how dun logs in to an OAuth- or OIDC-protected MCP server.
// A client registered with the identity provider by hand, with the redirect
// URI http://127.0.0.1:<redirect_port>/oauth/callback, is named here; with
// none, dun picks one (oauthauto.go).
type Spec struct {
	// ClientID is the registered client. Empty: dun's client metadata URL
	// where the server takes one, else a client dun registers (RFC 7591).
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret makes it a confidential client; empty is a public one
	// (PKCE only). "$NAME" or "${NAME}" is read from the environment, as
	// Bearer is; dun never writes a resolved secret.
	ClientSecret string `json:"client_secret,omitempty"`
	// Scopes to ask for. Empty: the 401's scope, else the server's
	// scopes_supported.
	Scopes []string `json:"scopes,omitempty"`
	// Issuer is the identity provider to log in with. Set, it skips
	// discovery: for a server that only validates tokens (Superset with
	// Google) or sits behind a gate. Empty: the server's 401 names its
	// protected-resource metadata (RFC 9728), which names the issuer.
	Issuer string `json:"issuer,omitempty"`
	// Audience, when set, is sent as audience= on the authorize request
	// (some IdPs need it to issue a token for an API).
	Audience string `json:"audience,omitempty"`
	// Resource is the RFC 8707 resource indicator sent on the authorize and
	// token requests. Empty: the server's resource when it was discovered,
	// and none with an explicit Issuer (Google and other IdPs that are not
	// MCP-aware reject unknown parameters). "none" never sends one; any
	// other value is sent as given.
	Resource string `json:"resource,omitempty"`
	// Send is which token goes in Authorization: "access_token" (default)
	// or "id_token" — for an IdP whose access tokens are opaque (Google),
	// where the server validates the ID token (aud = client_id).
	Send string `json:"send,omitempty"`
	// RedirectPort is the loopback port of the redirect URI; 0 is
	// DefaultRedirectPort. It is fixed so it can be registered exactly.
	RedirectPort int `json:"redirect_port,omitempty"`
	// Relay is the public callback base for a login completed on a remote
	// browser, e.g. "https://dun.iodesystems.com". Empty is today's
	// loopback mode. Set, the redirect URI is Relay + "/mcp/oauth/callback"
	// and no loopback listener is started: the host serving the callback
	// checks the state and calls Client.Deliver with the code.
	Relay string `json:"relay,omitempty"`
	// AuthParams are extra authorize-request parameters, e.g. Google's
	// access_type=offline and prompt=consent, without which it issues no
	// refresh token.
	AuthParams map[string]string `json:"auth_params,omitempty"`
}

// OAuth Send values.
const (
	SendAccessToken = "access_token"
	SendIDToken     = "id_token"
)
