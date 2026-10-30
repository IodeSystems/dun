package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// `dun oauth token` takes its server as --spec; a bad one is a usage error,
// and a server it cannot get a token for is a failure that says how to log in.
func TestOAuthCmd_SpecAndErrors(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir()) // no dund here: it asks the token file
	var out, errb bytes.Buffer
	if rc := oauthCmd([]string{"login"}, &out, &errb); rc != 2 || !strings.Contains(errb.String(), "usage") {
		t.Errorf("not token: %d %q", rc, errb.String())
	}
	errb.Reset()
	if rc := oauthCmd([]string{"token", "--spec", "!!"}, &out, &errb); rc != 2 {
		t.Errorf("bad spec: %d %q", rc, errb.String())
	}
	spec := dun.EncodeOAuthAuthSpec(dun.OAuthAuthSpec{ID: "eleven", URL: "http://127.0.0.1:1/mcp",
		OAuth: dun.OAuthSpec{ClientID: "c", Issuer: "http://127.0.0.1:1"}})
	errb.Reset()
	if rc := oauthCmd([]string{"token", "--spec", spec}, &out, &errb); rc != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "dun oauth token:") {
		t.Errorf("unreachable issuer: %d %q %q", rc, out.String(), errb.String())
	}
}
