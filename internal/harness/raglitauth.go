package harness

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
//
// Folded history used to go to the session's raglit branch,
// <ns>:sessions@<id>; it goes to dund's sessions index now
// (sessionindex_dund.go). Branches written before that are left to raglit's
// reaper.
//
// Keys live in memory; only the principal token is on disk. A session key
// expires after 7 days unused (raglit's reaper), so nothing here cleans up at
// close. /rag logout revokes the
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
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/proc"
)

// RaglitTokenFile is where /rag login keeps dun's raglit token.
func RaglitTokenFile() string { return filepath.Join(DunHome(), "oauth", "raglit.token") }

// RaglitJWTFile is where the remote-raglit login (cmd/dun/raglogin.go) keeps
// the OIDC access token it obtained for the human. A remote raglit daemon
// answers no request without this bearer; raglit has no device-flow of its
// own, so dun runs the login and hands the token to its raglit subprocess.
func RaglitJWTFile() string { return filepath.Join(DunHome(), "oauth", "raglit.jwt") }

// LoadRaglitJWT returns the stored remote-raglit bearer, "" when none.
func LoadRaglitJWT() string {
	b, err := os.ReadFile(RaglitJWTFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveRaglitJWT writes the remote-raglit bearer, 0600.
func SaveRaglitJWT(jwt string) error {
	file := RaglitJWTFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(jwt+"\n"), 0o600)
}

// RaglitTokenCommand is the value of RAGLIT_TOKEN_COMMAND for the raglit
// subprocess: a command whose stdout is the current bearer. The subprocess
// runs it on each start, so it re-fetches the token and picks up a refresh
// without dun restarting it. `dun rag token` prints the stored JWT
// (cmd/dun/raglogin.go). It resolves to dun's own path, so the command is
// found when dun was launched by absolute path and "dun" is not on the
// child's PATH.
func RaglitTokenCommand() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe + " rag token"
	}
	return "dun rag token"
}

// raglitRemoteEnv is the docs server's env for a remote raglit: the token
// command, so its daemon authenticates with the human's JWT. nil when there
// is no stored JWT, i.e. the server is not pointed at a remote daemon.
func raglitRemoteEnv() []string {
	if LoadRaglitJWT() == "" {
		return nil
	}
	return []string{"RAGLIT_TOKEN_COMMAND=" + RaglitTokenCommand()}
}

// raglitKeyIDFile holds the token's key id, which is what revoking takes.
func raglitKeyIDFile() string { return filepath.Join(DunHome(), "oauth", "raglit.key") }

// errRaglitLogin is a keyed raglit with no login yet.
var errRaglitLogin = errors.New("raglit needs a login — /rag login")

// runRaglit runs raglit's CLI in dir with env added, stdin as its input, and
// returns its stdout and stderr. A test replaces it.
var runRaglit = func(ctx context.Context, command, dir string, env []string, stdin string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := proc.Detach(exec.CommandContext(ctx, command, args...))
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
func raglitKeyed(s mcpserver.Server) (string, bool) {
	if s.ID != mcpserver.ServerDocs || s.URL != "" || filepath.Base(s.Command) != "raglit" {
		return "", false
	}
	command, err := mcpserver.FindCommand(s.Command)
	if err != nil || !raglitHasKeys(command) {
		return "", false
	}
	return command, true
}

// raglitAuth is the docs server's env for a keyed raglit: RAGLIT_TOKEN set
// to the session's key, minted on first use, plus RAGLIT_TOKEN_COMMAND when a
// remote-raglit JWT is stored (raglitRemoteEnv). nil, nil for any other
// server.
func (h *Harness) raglitAuth(ctx context.Context, s mcpserver.Server) ([]string, error) {
	var env []string
	if command, ok := raglitKeyed(s); ok {
		key, err := h.raglitSessionKey(ctx, command)
		if err != nil {
			return nil, err
		}
		env = append(env, "RAGLIT_TOKEN="+key)
	}
	// A remote raglit authenticates with the human's OIDC bearer, not a key
	// the local daemon minted: the token command is set whenever a JWT is
	// stored, whether or not this raglit is also keyed.
	if s.ID == mcpserver.ServerDocs {
		env = append(env, raglitRemoteEnv()...)
	}
	return env, nil
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
		// The project names the shared daemon's namespace; without it a
		// directory with no .raglit/config.json is refused ("no project name").
		"key", "mint", "--session", session, "--project", mcpserver.RaglitProject(r.cfg.Workspace), "--label", "dun session "+session)
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
func (h *Harness) raglitLogin(ctx context.Context, s mcpserver.Server) (string, error) {
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
	r.ragKey = ""
	r.ragMu.Unlock()
	return "logged in (dun's raglit token is in " + file + ")", nil
}

// raglitLogout revokes dun's token, and every key under it, and deletes it.
func (h *Harness) raglitLogout(ctx context.Context, s mcpserver.Server) (string, error) {
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
	r.ragKey = ""
	r.ragMu.Unlock()
	return msg + ")", nil
}
