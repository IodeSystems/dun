package dun

// raglitauth.go — /rag login, and the keys raglit's daemon wants.
//
// A raglit with keys (raglit never-open, 2026-10-30) answers no request
// without a credential. dun holds one of its own, never the admin's:
//
//   - /rag login mints an API token for principal "dun" with the caller's own
//     credential (on this machine raglit finds the local daemon's admin token;
//     elsewhere $RAGLIT_TOKEN) and keeps it in $DUN_HOME/oauth/raglit.token.
//     Until then the docs server does not start: "/rag login".
//   - Each root session mints a session key from it, for its own id. That key
//     is RAGLIT_TOKEN for the session's raglit mcp and for every raglit CLI
//     dun runs for it, so the model reads with the session's grants: the
//     project, and its own branches.
//   - The first fold mints a child key for the session's branch of the
//     sessions index, <ns>:sessions@<id>, and writes there: what a fold or a
//     recap took out of the context stays searchable (USER 2026-10-30: the
//     branch is owned by a child of the session's key, and exists from its
//     first write).
//
// Keys live in memory; only the principal token is on disk. A session key
// expires after 7 days unused and a branch after 7 days unwritten (raglit's
// reaper), so nothing here cleans up at close. /rag logout revokes the
// principal, and every key under it with it.
//
// A raglit without keys is left as it was: no login, no keys.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// RaglitTokenFile is where /rag login keeps dun's raglit token.
func RaglitTokenFile() string { return filepath.Join(DunHome(), "oauth", "raglit.token") }

// raglitKeyIDFile holds the token's key id, which is what revoking takes.
func raglitKeyIDFile() string { return filepath.Join(DunHome(), "oauth", "raglit.key") }

// errRaglitLogin is a keyed raglit with no login yet.
var errRaglitLogin = errors.New("raglit needs a login — /rag login")

// runRaglit runs raglit's CLI in dir with env added, stdin as its input, and
// returns its stdout and stderr. A test replaces it.
var runRaglit = func(ctx context.Context, command, dir string, env []string, stdin string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := detach(exec.CommandContext(ctx, command, args...))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("raglit %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), errb.String(), err
}

var raglitKeysCache sync.Map // resolved command → bool

// raglitHasKeys reports whether the raglit at command has keys: `raglit help
// key` succeeds on one and fails on an older raglit (raglit-a2, 2026-10-30;
// a bare `raglit key` fails on both), and its help names mint.
var raglitHasKeys = func(command string) bool {
	if v, ok := raglitKeysCache.Load(command); ok {
		return v.(bool)
	}
	out, errOut, err := runRaglit(context.Background(), command, "", nil, "", "help", "key")
	has := err == nil && strings.Contains(out+errOut, "mint")
	raglitKeysCache.Store(command, has)
	return has
}

// raglitKeyed is the resolved raglit command when s is the docs server and
// its raglit has keys.
func raglitKeyed(s Server) (string, bool) {
	// Opt-in until Carl hands never-open to dun (raglit-a2, 2026-10-30): a
	// dun rebuilt from this tree must not start asking for /rag login.
	if os.Getenv("DUN_RAGLIT_KEYS") != "1" {
		return "", false
	}
	if s.ID != ServerDocs || s.URL != "" || filepath.Base(s.Command) != "raglit" {
		return "", false
	}
	command, err := FindCommand(s.Command)
	if err != nil || !raglitHasKeys(command) {
		return "", false
	}
	return command, true
}

// raglitAuth is the docs server's env for a keyed raglit: RAGLIT_TOKEN set
// to the session's key, minted on first use. nil, nil for any other server.
func (h *Harness) raglitAuth(ctx context.Context, s Server) ([]string, error) {
	command, ok := raglitKeyed(s)
	if !ok {
		return nil, nil
	}
	key, err := h.raglitSessionKey(ctx, command)
	if err != nil {
		return nil, err
	}
	return []string{"RAGLIT_TOKEN=" + key}, nil
}

// raglitEnv is the env the docs server's CLI runs with: the session key once
// one is minted, else none.
func (h *Harness) raglitEnv() []string {
	r := h.ragRoot()
	r.ragMu.Lock()
	defer r.ragMu.Unlock()
	if r.ragKey == "" {
		return nil
	}
	return []string{"RAGLIT_TOKEN=" + r.ragKey}
}

// ragRoot is the harness whose session a child's keys and branch belong to.
func (h *Harness) ragRoot() *Harness {
	for h.parent != nil {
		h = h.parent
	}
	return h
}

// raglitSession is the session id raglit knows this session by: the root's
// session file's name, which a resume keeps.
func (h *Harness) raglitSession() string {
	r := h.ragRoot()
	r.ragMu.Lock()
	defer r.ragMu.Unlock()
	if r.ragSession == "" {
		r.ragSession = strings.TrimSuffix(filepath.Base(r.cfg.SessionFile), ".jsonl")
		if r.cfg.SessionFile == "" {
			r.ragSession = "mem-" + agent.NewID()
		}
	}
	return r.ragSession
}

// raglitSessionKey mints the session's key from dun's token, once.
func (h *Harness) raglitSessionKey(ctx context.Context, command string) (string, error) {
	session := h.raglitSession()
	r := h.ragRoot()
	r.ragMu.Lock()
	defer r.ragMu.Unlock()
	if r.ragKey != "" {
		return r.ragKey, nil
	}
	token := loadRaglitToken()
	if token == "" {
		return "", errRaglitLogin
	}
	out, _, err := runRaglit(ctx, command, r.cfg.Workspace, []string{"RAGLIT_TOKEN=" + token}, "",
		"key", "mint", "--session", session, "--label", "dun session "+session)
	if err != nil {
		return "", fmt.Errorf("%w — /rag login again if dun's token was revoked", err)
	}
	if r.ragKey = strings.TrimSpace(out); r.ragKey == "" {
		return "", errors.New("raglit key mint --session printed no key")
	}
	return r.ragKey, nil
}

// RaglitTokenEnv is RAGLIT_TOKEN set to dun's token, for a raglit run that
// is no session's (setup, doctor, a worktree's index drop); nil when not
// logged in, and raglit then finds its own credential as before.
func RaglitTokenEnv() []string {
	if t := loadRaglitToken(); t != "" {
		return []string{"RAGLIT_TOKEN=" + t}
	}
	return nil
}

func loadRaglitToken() string {
	b, err := os.ReadFile(RaglitTokenFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// raglitLogin mints dun's raglit token with the caller's own credential.
func (h *Harness) raglitLogin(ctx context.Context, s Server) (string, error) {
	command, ok := raglitKeyed(s)
	if !ok {
		return "", fmt.Errorf("%s is not a raglit with keys: nothing to log in to", s.ID)
	}
	file := RaglitTokenFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	// --json with --out: the token goes only to the file, and stdout is
	// {"key": {"id", ...}} (raglit d7a5e69). The id is what revoking takes.
	out, _, err := runRaglit(ctx, command, h.cfg.Workspace, nil, "",
		"key", "mint", "--principal", "dun", "--label", "dun@"+host, "--out", file, "--json")
	if err != nil {
		return "", err
	}
	var minted struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if json.Unmarshal([]byte(out), &minted) == nil && minted.Key.ID != "" {
		_ = os.WriteFile(raglitKeyIDFile(), []byte(minted.Key.ID+"\n"), 0o600)
	}
	// The next start mints a session key from the new token.
	r := h.ragRoot()
	r.ragMu.Lock()
	r.ragKey, r.ragBranchKey = "", ""
	r.ragMu.Unlock()
	return "logged in (dun's raglit token is in " + file + ")", nil
}

// raglitLogout revokes dun's token, and every key under it, and deletes it.
func (h *Harness) raglitLogout(ctx context.Context, s Server) (string, error) {
	command, ok := raglitKeyed(s)
	if !ok {
		return "", fmt.Errorf("%s is not a raglit with keys", s.ID)
	}
	token := loadRaglitToken()
	if token == "" {
		return "not logged in", nil
	}
	msg := "logged out (token deleted"
	if b, err := os.ReadFile(raglitKeyIDFile()); err == nil {
		id := strings.TrimSpace(string(b))
		if _, _, err := runRaglit(ctx, command, h.cfg.Workspace, []string{"RAGLIT_TOKEN=" + token}, "", "key", "revoke", id); err == nil {
			msg += " and revoked, with its session keys"
		} else {
			msg += "; NOT revoked: " + oneLine(err.Error(), 200)
		}
	}
	_ = os.Remove(RaglitTokenFile())
	_ = os.Remove(raglitKeyIDFile())
	r := h.ragRoot()
	r.ragMu.Lock()
	r.ragKey, r.ragBranchKey = "", ""
	r.ragMu.Unlock()
	return msg + ")", nil
}

// sessionIndex is where folds and recaps put what left the context: the
// session's branch of the sessions index. nil unless the docs server is a
// keyed raglit with a session key.
func (h *Harness) sessionIndex() agent.SessionIndex {
	h.srvMu.Lock()
	s, ok := h.spec(ServerDocs)
	h.srvMu.Unlock()
	if !ok {
		return nil
	}
	command, keyed := raglitKeyed(s)
	if !keyed {
		return nil
	}
	return &raglitSessionIndex{h: h.ragRoot(), command: command}
}

// raglitSessionIndex writes to <ns>:sessions@<session> with the branch's
// own key. Reading needs nothing of it: with the session key, raglit's
// default read includes the branches that key and its children own
// (raglit 080a46e), so Search is not used.
type raglitSessionIndex struct {
	h       *Harness
	command string
}

func (x *raglitSessionIndex) branch() string { return "sessions@" + x.h.raglitSession() }

// branchKey mints the branch's child key from the session key, once.
func (x *raglitSessionIndex) branchKey(ctx context.Context) (string, error) {
	r := x.h
	r.ragMu.Lock()
	sessionKey, have := r.ragKey, r.ragBranchKey
	r.ragMu.Unlock()
	if have != "" {
		return have, nil
	}
	if sessionKey == "" {
		return "", errors.New("no raglit session key yet (the docs server has not started)")
	}
	out, _, err := runRaglit(ctx, x.command, r.cfg.Workspace, []string{"RAGLIT_TOKEN=" + sessionKey}, "",
		"key", "mint", "--branch", x.branch(), "--label", "dun folds "+x.h.raglitSession())
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(out)
	if key == "" {
		return "", errors.New("raglit key mint --branch printed no key")
	}
	r.ragMu.Lock()
	r.ragBranchKey = key
	r.ragMu.Unlock()
	return key, nil
}

// Put queues the document and returns: the ingest runs beside the turn, and
// a failure is logged to the docs server's activity, not the fold's.
func (x *raglitSessionIndex) Put(ctx context.Context, d agent.SessionDoc) error {
	go func() {
		if err := x.put(context.WithoutCancel(ctx), d); err != nil {
			x.h.noteServer(ServerDocs, "session history: %s", oneLine(err.Error(), 300))
		}
	}()
	return nil
}

func (x *raglitSessionIndex) put(ctx context.Context, d agent.SessionDoc) error {
	key, err := x.branchKey(ctx)
	if err != nil {
		return err
	}
	ident, err := json.Marshal(map[string]string{"name": d.Name, "summary": d.Summary, "kind": d.Kind})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "dun-identity-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(ident); err != nil {
		f.Close()
		return err
	}
	f.Close()
	_, _, err = runRaglit(ctx, x.command, x.h.cfg.Workspace, []string{"RAGLIT_TOKEN=" + key}, d.Body,
		"ingest", "--index", x.branch(), "--name", d.Key, "--text-file", "-", "--identity", f.Name())
	return err
}

func (x *raglitSessionIndex) Search(context.Context, string, int) ([]agent.SessionHit, error) {
	return nil, errors.New("a session's raglit search reads its branch; search through the docs server")
}

func (x *raglitSessionIndex) Close(context.Context) error { return nil } // raglit's reaper

// lateSessionIndex is the shaper's index: built with the harness, before its
// servers are known, so it asks for the session index at each fold.
type lateSessionIndex struct{ h *Harness }

func (l lateSessionIndex) Put(ctx context.Context, d agent.SessionDoc) error {
	if x := l.h.sessionIndex(); x != nil {
		return x.Put(ctx, d)
	}
	return nil
}

func (l lateSessionIndex) Search(context.Context, string, int) ([]agent.SessionHit, error) {
	return nil, nil
}

func (l lateSessionIndex) Close(context.Context) error { return nil }
