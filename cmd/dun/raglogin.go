package main

// raglogin.go — `dun rag token` and `dun rag login`, the remote-raglit
// credential.
//
// A remote raglit daemon answers no request without an OIDC bearer, and it
// has no device-flow of its own: dun runs the login and hands the token to
// its raglit subprocess through RAGLIT_TOKEN_COMMAND (harness/raglitauth.go),
// which is a command whose stdout is the current bearer. That command is
// `dun rag token`.
//
//   - `dun rag token` prints the stored JWT (harness.RaglitJWTFile) to stdout
//     and nothing else; the raglit subprocess reads it on each start. No JWT
//     → nothing on stdout, exit 1, the reason on stderr.
//   - `dun rag login` runs the browser PKCE flow against the daemon's issuer
//     (internal/oauth, the same machinery a remote MCP server uses), and keeps
//     the access token. It is the interactive half; `dun rag token` is what
//     the subprocess calls on its own.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/oauth"
	"github.com/iodesystems/dun/internal/openurl"
)

// ragLoginTimeout bounds `dun rag login` from the CLI side. openurl.Open is
// best-effort and non-blocking (it detaches the browser), so the real bound
// is oauthLoginTimeout inside c.Login; this outer bound is a second ceiling so
// the CLI can never outlive it.
var ragLoginTimeout = 10 * time.Minute

// ragCmd is `dun rag <token|login>`: the remote-raglit credential.
//
//   - token: print the stored bearer (harness.RaglitJWTFile) to stdout and
//     nothing else. This is RAGLIT_TOKEN_COMMAND for the raglit subprocess
//     (harness.RaglitTokenCommand), which runs it on each start and re-reads
//     the token, picking up a refresh without dun restarting it. No JWT →
//     nothing on stdout, exit 1, the reason on stderr.
//   - login: the interactive browser PKCE flow against the daemon's issuer;
//     it stores the access token that `token` will later print.
func ragCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dun rag token | dun rag login <raglit-url> [issuer]")
		return 2
	}
	switch args[0] {
	case "token":
		tok := ragToken()
		if tok == "" {
			fmt.Fprintln(stderr, "no raglit login — run: dun rag login <raglit-url>")
			return 1
		}
		fmt.Fprintln(stdout, tok)
		return 0
	case "login":
		return ragLogin(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown rag command %q\n", args[0])
		return 2
	}
}

// ragToken is the current remote-raglit bearer for `dun rag token`. It
// consults the oauth client for id "raglit" first — its Authorization refreshes
// an expiring access token — and falls back to the stored JWT file. A login
// (ragLogin) keeps both in step: it stores the access token in the file for a
// bare print, and the oauth client's token file is what a refresh comes from.
// "" when neither has one.
func ragToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := oauth.NewClient("raglit", "", oauth.Spec{}, nil, nil)
	if v, err := c.Authorization(ctx); err == nil {
		if t := strings.TrimPrefix(v, "Bearer "); t != "" {
			return t
		}
	}
	return harness.LoadRaglitJWT()
}

// ragLogin is `dun rag login <raglit-url> [issuer]`: the browser PKCE flow
// for a remote raglit. The access token is what the daemon validates
// (JWT-type, aud = its auth.audience). The flow is bounded by
// ragLoginTimeout from the CLI and by oauthLoginTimeout inside c.Login;
// openurl.Open is non-blocking, so neither waits on the browser process.
func ragLogin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dun rag login <raglit-url> [issuer]")
		return 2
	}
	url := args[0]
	issuer := ""
	if len(args) > 1 {
		issuer = args[1]
	}
	spec := oauth.Spec{Issuer: issuer}
	c := oauth.NewClient("raglit", url, spec, nil, openurl.Open)
	ctx, cancel := context.WithTimeout(context.Background(), ragLoginTimeout)
	defer cancel()
	if err := c.Login(ctx, ""); err != nil {
		fmt.Fprintf(stderr, "raglit login: %v\n", err)
		return 1
	}
	// Store the access token as the bearer `token` will later print.
	// Authorization returns "Bearer <token>"; keep the token itself.
	tok, _ := c.Authorization(ctx)
	tok = strings.TrimPrefix(tok, "Bearer ")
	if tok == "" {
		fmt.Fprintln(stderr, "raglit login: the flow finished without an access token")
		return 1
	}
	if err := harness.SaveRaglitJWT(tok); err != nil {
		fmt.Fprintf(stderr, "raglit login: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "logged in (the raglit token is in %s)\n", harness.RaglitJWTFile())
	return 0
}
