package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
)

// `dun oauth token --spec <b64> [--session <id>]` prints an OAuth MCP
// server's bearer token: mcpshell's auth_cmd for a scripted OAuth server
// (shellhost.go). It asks dund, which holds the token and refreshes it
// (dun/oauthbroker.go); with no dund, it refreshes from the token file
// itself. MCPSHELL_AUTH_RETRY=1 is mcpshell saying the server refused the
// last token: refresh, else log in.

// oauthTokenTimeout stays under mcpshell's 30 s for one auth_cmd run.
const oauthTokenTimeout = 25 * time.Second

func oauthCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "token" {
		fmt.Fprintln(stderr, "usage: dun oauth token --spec <base64 json> [--session <id>]")
		return 2
	}
	fs := flag.NewFlagSet("oauth token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	specB64 := fs.String("spec", "", "the server's id, url and oauth block, as base64 JSON")
	session := fs.String("session", "", "the session a login's URL is shown in")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	spec, err := dun.DecodeOAuthAuthSpec(*specB64)
	if err != nil {
		fmt.Fprintln(stderr, "dun oauth token:", err)
		return 2
	}
	retry := os.Getenv("MCPSHELL_AUTH_RETRY") == "1"
	ctx, cancel := context.WithTimeout(context.Background(), oauthTokenTimeout)
	defer cancel()
	auth, err := oauthFromDund(ctx, spec, *session, retry)
	if errors.Is(err, errNoDund) {
		s := dun.Server{ID: spec.ID, URL: spec.URL, OAuth: &spec.OAuth}
		auth, err = dun.OAuthTokenHere(ctx, s, retry,
			func(id, url string) { fmt.Fprintln(stderr, loginLine(id, url)) }, nil)
	}
	if err == nil && auth == "" {
		err = fmt.Errorf("%s: no token — log in with /mcp login %s", spec.ID, spec.ID)
	}
	if err != nil {
		fmt.Fprintln(stderr, "dun oauth token:", err)
		return 1
	}
	fmt.Fprint(stdout, strings.TrimPrefix(auth, "Bearer "))
	return 0
}

var errNoDund = errors.New("no dund")

// oauthFromDund asks dund over its socket; errNoDund when none answers.
func oauthFromDund(ctx context.Context, spec dun.OAuthAuthSpec, session string, retry bool) (string, error) {
	c, err := net.DialTimeout("unix", dundcli.Socket(dun.DunHome()), 2*time.Second)
	if err != nil {
		return "", errNoDund
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	o := spec.OAuth
	o.ClientSecret = resolveRef(o.ClientSecret)
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	ask := func(op string) (string, error) {
		req := proto.OAuthReq{Op: op, ID: spec.ID, URL: spec.URL, Spec: dun.MarshalOAuthSpec(o)}
		if op == "unauthorized" {
			if r := os.Getenv("MCPSHELL_AUTH_REJECTED"); r != "" {
				req.Rejected = "Bearer " + r
			}
		}
		cmd := proto.NewCommand(op, &proto.OAuthCmd{OAuthReq: req})
		cmd.Session = session
		if err := enc.Encode(cmd); err != nil {
			return "", err
		}
		for {
			var ev proto.Event
			if err := dec.Decode(&ev); err != nil {
				return "", err
			}
			if ev.Re != op {
				continue
			}
			switch p := ev.Payload.(type) {
			case *proto.Ack:
				if p.Error != "" {
					return "", errors.New(p.Error)
				}
			case *proto.OAuthResult:
				if p.Error != "" {
					return p.Authorization, errors.New(p.Error)
				}
				return p.Authorization, nil
			}
		}
	}
	op := "token"
	if retry {
		op = "unauthorized"
	}
	auth, err := ask(op)
	if err == nil && auth == "" {
		auth, err = ask("ensure")
	}
	return auth, err
}

// resolveRef reads a "$NAME" or "${NAME}" value from the environment.
func resolveRef(v string) string {
	n := strings.TrimPrefix(v, "$")
	if n == v {
		return v
	}
	return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(n, "{"), "}"))
}
