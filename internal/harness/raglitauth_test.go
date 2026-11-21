package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// A raglit with keys (raglit never-open): dun logs in with its own token,
// mints a key per session and one per session branch, and writes folds into
// the branch. A fake CLI records each run; nothing reaches a daemon.

type raglitRun struct {
	env   []string
	stdin string
	args  []string
}

func (r raglitRun) token() string {
	for _, e := range r.env {
		if v, ok := strings.CutPrefix(e, "RAGLIT_TOKEN="); ok {
			return v
		}
	}
	return ""
}

// fakeRaglit points $DUN_RAGLIT at a raglit with keys and answers its key and
// ingest commands; runs records every call.
func fakeRaglit(t *testing.T) (*[]raglitRun, mcpserver.Server) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "raglit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUN_RAGLIT", filepath.Join(bin, "raglit")) // dun runs its own raglit, not PATH's
	var runs []raglitRun
	oldRun, oldHas := runRaglit, raglitHasKeys
	runRaglit = func(_ context.Context, _, _ string, env []string, stdin string, args ...string) (string, string, error) {
		r := raglitRun{env: env, stdin: stdin, args: args}
		runs = append(runs, r)
		a := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(a, "key mint --principal"):
			out := args[len(args)-2] // --out FILE --json
			if err := os.WriteFile(out, []byte("tok-dun\n"), 0o600); err != nil {
				return "", "", err
			}
			return `{"key":{"id":"k_dun","kind":"principal","principal":"dun"}}`, "wrote principal key k_dun (dun) to " + out + "\n", nil
		case strings.HasPrefix(a, "key mint --session"):
			if r.token() != "tok-dun" {
				return "", "", errors.New("401")
			}
			return "key-session\n", "", nil
		}
		return "", "", nil
	}
	raglitHasKeys = func(string) bool { return true }
	t.Cleanup(func() { runRaglit, raglitHasKeys = oldRun, oldHas })
	return &runs, mcpserver.Server{ID: mcpserver.ServerDocs, Command: "raglit"}
}

func TestRaglitAuth_NeedsLoginThenMintsOneSessionKey(t *testing.T) {
	runs, docs := fakeRaglit(t)
	h := testHarness(t)
	if _, err := h.raglitAuth(context.Background(), docs); !errors.Is(err, errRaglitLogin) {
		t.Fatalf("no login: %v", err)
	}
	if _, err := h.raglitLogin(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(RaglitTokenFile()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(raglitKeyIDFile()); strings.TrimSpace(string(b)) != "k_dun" {
		t.Errorf("key id %q", b)
	}
	if (*runs)[0].token() != "" {
		t.Error("login must mint with the caller's own credential, not a token of dun's")
	}
	for range 2 {
		env, err := h.raglitAuth(context.Background(), docs)
		if err != nil || len(env) != 1 || env[0] != "RAGLIT_TOKEN=key-session" {
			t.Fatalf("auth env %v, %v", env, err)
		}
	}
	var mints []raglitRun
	for _, r := range *runs {
		if strings.HasPrefix(strings.Join(r.args, " "), "key mint --session") {
			mints = append(mints, r)
		}
	}
	if len(mints) != 1 || mints[0].args[3] != "s" {
		t.Errorf("session mints %v, want one for session s", mints)
	}
	if a := mints[0].args; len(a) < 6 || a[4] != "--project" || a[5] == "" {
		t.Errorf("session mint %v must name the project (the daemon refuses a mint without one)", a)
	}
	if got := h.raglitEnv(); len(got) != 1 || got[0] != "RAGLIT_TOKEN=key-session" {
		t.Errorf("CLI env %v", got)
	}
}

// A raglit without keys is left as it was.
func TestRaglitAuth_OlderRaglitIsUntouched(t *testing.T) {
	_, docs := fakeRaglit(t)
	raglitHasKeys = func(string) bool { return false }
	h := testHarness(t)
	if env, err := h.raglitAuth(context.Background(), docs); env != nil || err != nil {
		t.Errorf("%v, %v", env, err)
	}
	if env, err := h.raglitAuth(context.Background(), mcpserver.Server{ID: mcpserver.ServerCode, Command: "raglit"}); env != nil || err != nil {
		t.Errorf("another server: %v, %v", env, err)
	}
}

// A keyed local raglit and a stored remote JWT both present: the docs server
// gets the session key AND the remote token command, so its daemon can be the
// shared one and raglit's EnvTokenSource still prefers the local key.
func TestRaglitAuth_LocalKeyAndRemoteJWT(t *testing.T) {
	_, docs := fakeRaglit(t)
	SaveRaglitJWT("remote-jwt")
	h := testHarness(t)
	if _, err := h.raglitLogin(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	env, err := h.raglitAuth(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	var haveKey, haveCmd bool
	for _, e := range env {
		if e == "RAGLIT_TOKEN=key-session" {
			haveKey = true
		}
		if v, ok := strings.CutPrefix(e, "RAGLIT_TOKEN_COMMAND="); ok && strings.HasSuffix(v, "rag token") {
			haveCmd = true
		}
	}
	if !haveKey {
		t.Errorf("env %v has no RAGLIT_TOKEN=key-session", env)
	}
	if !haveCmd {
		t.Errorf("env %v has no RAGLIT_TOKEN_COMMAND", env)
	}
}

func TestRaglitLogout_RevokesAndDeletes(t *testing.T) {
	runs, docs := fakeRaglit(t)
	h := testHarness(t)
	if _, err := h.raglitLogin(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	msg, err := h.raglitLogout(context.Background(), docs)
	if err != nil || !strings.Contains(msg, "revoked") {
		t.Fatalf("%q %v", msg, err)
	}
	last := (*runs)[len(*runs)-1]
	if strings.Join(last.args, " ") != "key revoke k_dun" || last.token() != "tok-dun" {
		t.Errorf("revoke %v with %q", last.args, last.token())
	}
	if loadRaglitToken() != "" {
		t.Error("the token is still on disk")
	}
	if _, err := h.raglitAuth(context.Background(), docs); !errors.Is(err, errRaglitLogin) {
		t.Errorf("after logout: %v", err)
	}
}

// A child's folds belong to its root's session: same id, same keys.
func TestRaglitSession_IsTheRoots(t *testing.T) {
	root := testHarness(t)
	root.cfg.SessionFile = filepath.Join(t.TempDir(), "20261030-120000.jsonl")
	kid := testHarness(t)
	kid.parent = root
	if got := kid.raglitSession(); got != "20261030-120000" {
		t.Errorf("child session %q", got)
	}
	root.ragKey = "key-session"
	if env := kid.raglitEnv(); len(env) != 1 || env[0] != "RAGLIT_TOKEN=key-session" {
		t.Errorf("child env %v", env)
	}
}

func TestRaglitJWTFile_SavesAndLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	path := RaglitJWTFile()
	if got := filepath.Base(path); got != "raglit.jwt" {
		t.Fatalf("RaglitJWTFile() = %q, want raglit.jwt", path)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("JWT file exists before Save: %v", err)
	}

	SaveRaglitJWT("eyJhbGciOiJSUzI1NiJ9.abc.def")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("JWT file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("JWT file perms = %o, want 0600", perm)
	}

	if got := LoadRaglitJWT(); got != "eyJhbGciOiJSUzI1NiJ9.abc.def" {
		t.Errorf("LoadRaglitJWT() = %q, want the saved JWT", got)
	}
}

func TestRaglitJWTFile_Absent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	if got := LoadRaglitJWT(); got != "" {
		t.Errorf("LoadRaglitJWT (absent) = %q, want empty", got)
	}
}

func TestRaglitRemoteEnv_ReturnsCommandWhenJWTStored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	SaveRaglitJWT("jwt-value")

	env := raglitRemoteEnv()
	if len(env) == 0 {
		t.Fatal("raglitRemoteEnv() = empty, want [RAGLIT_TOKEN_COMMAND=…]")
	}
	v, ok := strings.CutPrefix(env[0], "RAGLIT_TOKEN_COMMAND=")
	if !ok || v != RaglitTokenCommand() {
		t.Errorf("raglitRemoteEnv()[0] = %q, want %q", env[0], "RAGLIT_TOKEN_COMMAND="+RaglitTokenCommand())
	}
}

func TestRaglitRemoteEnv_NilWhenNoJWT(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)

	if env := raglitRemoteEnv(); env != nil {
		t.Errorf("raglitRemoteEnv() = %v, want nil", env)
	}
}
