package dun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
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

// fakeRaglit puts a raglit with keys on PATH and answers its key and ingest
// commands; runs records every call.
func fakeRaglit(t *testing.T) (*[]raglitRun, Server) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "raglit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		case strings.HasPrefix(a, "key mint --branch"):
			if r.token() != "key-session" {
				return "", "", errors.New("403")
			}
			return "key-branch\n", "", nil
		}
		return "", "", nil
	}
	raglitHasKeys = func(string) bool { return true }
	t.Cleanup(func() { runRaglit, raglitHasKeys = oldRun, oldHas })
	return &runs, Server{ID: ServerDocs, Command: "raglit"}
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
	if env, err := h.raglitAuth(context.Background(), Server{ID: ServerCode, Command: "raglit"}); env != nil || err != nil {
		t.Errorf("another server: %v, %v", env, err)
	}
}

// A fold goes into the session's branch with the branch's own key, minted
// once from the session key, with the summary as its identity.
func TestRaglitSessionIndex_PutsIntoTheBranch(t *testing.T) {
	runs, docs := fakeRaglit(t)
	h := testHarness(t)
	if _, err := h.raglitLogin(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raglitAuth(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	x := &raglitSessionIndex{h: h, command: "raglit"}
	var ident []string
	runRaglitInner := runRaglit
	runRaglit = func(ctx context.Context, c, d string, env []string, stdin string, args ...string) (string, string, error) {
		for i, a := range args {
			if a == "--identity" {
				b, _ := os.ReadFile(args[i+1])
				ident = append(ident, string(b))
			}
		}
		return runRaglitInner(ctx, c, d, env, stdin, args...)
	}
	for _, key := range []string{"fold-1", "fold-2"} {
		if err := x.put(context.Background(), agent.SessionDoc{Key: key, Kind: "fold", Name: "find the port", Summary: "S", Body: "1. [tool_result] 7420\n"}); err != nil {
			t.Fatal(err)
		}
	}
	var branchMints, ingests []raglitRun
	for _, r := range *runs {
		switch a := strings.Join(r.args, " "); {
		case strings.HasPrefix(a, "key mint --branch sessions@s"):
			branchMints = append(branchMints, r)
		case strings.HasPrefix(a, "ingest"):
			ingests = append(ingests, r)
		}
	}
	if len(branchMints) != 1 {
		t.Errorf("%d branch mints, want 1", len(branchMints))
	}
	if len(ingests) != 2 {
		t.Fatalf("%d ingests", len(ingests))
	}
	in := ingests[0]
	if a := strings.Join(in.args, " "); !strings.HasPrefix(a, "ingest --index sessions@s --name fold-1 --text-file - --identity ") {
		t.Errorf("ingest %q", a)
	}
	if in.token() != "key-branch" || in.stdin != "1. [tool_result] 7420\n" {
		t.Errorf("ingest token %q stdin %q", in.token(), in.stdin)
	}
	if len(ident) != 2 || !strings.Contains(ident[0], `"summary":"S"`) || !strings.Contains(ident[0], `"kind":"fold"`) {
		t.Errorf("identity %v", ident)
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
