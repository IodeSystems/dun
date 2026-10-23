package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Shell-placed MCP servers.
//
// A server dun connects to directly puts every one of its tools' schemas into
// every request. mcpshell can HOST upstream servers instead: they become
// namespaced commands inside its one eval tool, found with help("<ns>"), and a
// namespace over 20 commands costs one line. A user-added server chooses its
// placement (ServerSpec.Via); direct is the default.
//
// A shell-placed server is never started by dun. The shell's launch carries
// it: startServer writes the hosted servers to a {"mcpServers": …} file and
// adds --mcp <file> to mcpshell's args. Any change to the hosted set restarts
// the shell.
//
// What a hosted server loses: dun's docs indexing of its resources (/mcp
// refresh), its prompts and skills (discoverCatalog runs only on a direct
// start), its per-tool deny-list (mcpshell has none) and dun's per-tool
// renderers — its calls show as eval.

// shellMCPFile is the generated mcpshell config, in the project's state dir.
const shellMCPFile = "shell-mcp.json"

// ShellNamespace is the name mcpshell gives a hosted server: an identifier,
// because the model calls it as <ns>.<tool>(…) — "corp-docs" would parse as
// a subtraction.
func ShellNamespace(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	ns := b.String()
	if ns == "" || (ns[0] >= '0' && ns[0] <= '9') {
		ns = "_" + ns
	}
	return ns
}

// mcpshellServer is one entry of mcpshell's --mcp JSON (mcpshell
// cmd/mcpshell/main.go parseMCPConfig).
type mcpshellServer struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	AuthCmd string            `json:"auth_cmd,omitempty"`
}

// envRef matches a bearer that is only a variable reference: "$NAME" or
// "${NAME}".
var envRef = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// mcpshellConfig renders the hosted servers as mcpshell's --mcp JSON.
//
// A bearer that is a "$NAME" reference becomes auth_cmd printing that
// variable: mcpshell inherits dun's environment, so the token is read when it
// connects and never written out. Any other bearer, and every header, goes in
// resolved — mcpshell does not expand variables in headers — which is why the
// file is 0600, like the mcp.json the value came from.
func mcpshellConfig(hosted []Server) ([]byte, error) {
	out := map[string]mcpshellServer{}
	for _, s := range hosted {
		ns := ShellNamespace(s.ID)
		if _, dup := out[ns]; dup {
			return nil, fmt.Errorf("dun: servers %q and another both map to shell namespace %q — rename one", s.ID, ns)
		}
		var e mcpshellServer
		if s.URL != "" {
			e.URL = s.URL
			headers := map[string]string{}
			for k, v := range s.Headers {
				headers[k] = v
			}
			if m := envRef.FindStringSubmatch(s.Bearer); m != nil {
				name := m[1] + m[2]
				delete(headers, "Authorization")
				if os.Getenv(name) != "" { // unset: no Authorization, as direct
					e.AuthCmd = `printf '%s' "$` + name + `"`
				}
			}
			if len(headers) > 0 {
				e.Headers = headers
			}
		} else {
			e.Command, e.Args = s.Command, s.Args
			if len(s.Env) > 0 {
				e.Env = map[string]string{}
				for _, kv := range s.Env {
					k, v, _ := strings.Cut(kv, "=")
					e.Env[k] = v
				}
			}
		}
		out[ns] = e
	}
	return json.MarshalIndent(map[string]any{"mcpServers": out}, "", "  ")
}

// hostedSpecs lists the shell-placed servers that are on. Caller holds srvMu.
func (h *Harness) hostedSpecs() []Server {
	var out []Server
	for _, s := range h.specs {
		if s.Via == ViaShell && s.Autostart {
			out = append(out, s)
		}
	}
	return out
}

// shellArgs is the shell's argv: its configured args, plus --mcp <file> when
// any server is placed in it, and always a file toolkit. A shell that is not
// mcpshell is left alone.
func (h *Harness) shellArgs(s Server) []string {
	args := h.shellArgsHosted(s)
	if filepath.Base(s.Command) == "mcpshell" {
		args = withFilesDir(args, h.cfg.Workspace)
	}
	return args
}

// overflowEnv names the dir mcpshell saves over-limit eval output to; the
// truncated response points at the file and read()/grep() accept its path.
const overflowEnv = "MCPSHELL_TMP_OVERFLOW_DIR"

// shellEnv adds MCPSHELL_TMP_OVERFLOW_DIR to an mcpshell shell's env, unless
// the config already sets it. A shell that is not mcpshell is left alone.
func (h *Harness) shellEnv(s Server, env []string) []string {
	if filepath.Base(s.Command) != "mcpshell" {
		return env
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, overflowEnv+"=") {
			return env
		}
	}
	return append(env, overflowEnv+"="+h.overflowDir())
}

// overflowDir is this session's dir for the shell's saved output: beside the
// session file and keyed by its id, so a shell restart or a resume finds the
// same files; else a temp dir per process. mcpshell creates it.
func (h *Harness) overflowDir() string {
	if h.cfg.SessionFile == "" {
		return filepath.Join(os.TempDir(), "dun-overflow", strconv.Itoa(os.Getpid()))
	}
	id := strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
	return filepath.Join(filepath.Dir(h.cfg.SessionFile), "overflow", id)
}

// withFilesDir adds `--files-dir <root>` to mcpshell's args unless they name
// one: a configured shell entry replaces the default args wholesale, and one
// that set only --web (say) lost the file toolkit with them (USER 2026-10-22).
func withFilesDir(args []string, root string) []string {
	if root == "" {
		return args
	}
	for _, a := range args {
		if a == "--files-dir" || a == "-files-dir" || strings.HasPrefix(a, "--files-dir=") || strings.HasPrefix(a, "-files-dir=") {
			return args
		}
	}
	return append(append([]string{}, args...), "--files-dir", root)
}

func (h *Harness) shellArgsHosted(s Server) []string {
	h.srvMu.Lock()
	hosted := h.hostedSpecs()
	h.srvMu.Unlock()
	if len(hosted) == 0 || filepath.Base(s.Command) != "mcpshell" {
		// No stale copy of a token outlives its server. Resolve, not
		// Ensure: a shell start must not register a project.
		if p := ResolveProject(h.configDir()); p != nil {
			_ = os.Remove(filepath.Join(p.WriteDir(), shellMCPFile))
		}
		if len(hosted) > 0 {
			log.Printf("dun: shell is %q, not mcpshell — it cannot host %d server(s)", s.Command, len(hosted))
		}
		return s.Args
	}
	path := h.shellMCPPath()
	if path == "" {
		log.Printf("dun: no project state dir — the shell starts without its hosted servers")
		return s.Args
	}
	b, err := mcpshellConfig(hosted)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = writeFileAtomic(path, append(b, '\n'), 0o600)
	}
	if err != nil {
		log.Printf("dun: the shell starts without its hosted servers: %v", err)
		return s.Args
	}
	return append(append([]string{}, s.Args...), "--mcp", path)
}

// shellMCPPath is where the generated file goes: the project's state dir,
// never the repo. "" when there is none.
func (h *Harness) shellMCPPath() string {
	p, err := EnsureProject(h.configDir())
	if err != nil {
		return ""
	}
	return filepath.Join(p.WriteDir(), shellMCPFile)
}

// configDir is where this session's config files are looked for.
func (h *Harness) configDir() string {
	if h.cfg.ConfigDir != "" {
		return h.cfg.ConfigDir
	}
	return h.cfg.Workspace
}

// hostedState is what the running shell said about one hosted server.
type hostedState struct {
	Connected bool
	Tools     int
	Err       string
}

// The lines mcpshell writes to stderr per upstream (mcp/toolkit.go
// InstallClients). Pinned by a test; a reworded line reads as "unknown".
var (
	reUpConnected = regexp.MustCompile(`upstream "([^"]+)" connected \(.*\) — (\d+) tool`)
	reUpFailed    = regexp.MustCompile(`(?:failed to connect upstream "([^"]+)": |upstream "([^"]+)" tools/list failed: |skipping upstream "([^"]+)" — )(.*)`)
)

// parseHosted reads the shell's stderr for each hosted server's fate. A
// server with no line (the tail is bounded) is absent from the result.
func parseHosted(stderr string, hosted []Server) map[string]hostedState {
	byNS := map[string]string{}
	for _, s := range hosted {
		byNS[ShellNamespace(s.ID)] = s.ID
	}
	out := map[string]hostedState{}
	for _, line := range strings.Split(stderr, "\n") {
		if m := reUpConnected.FindStringSubmatch(line); m != nil {
			if id, ok := byNS[m[1]]; ok {
				n, _ := strconv.Atoi(m[2])
				out[id] = hostedState{Connected: true, Tools: n}
			}
			continue
		}
		if m := reUpFailed.FindStringSubmatch(line); m != nil {
			if id, ok := byNS[m[1]+m[2]+m[3]]; ok {
				out[id] = hostedState{Err: strings.TrimSpace(m[4])}
			}
		}
	}
	return out
}

// readHosted records what the just-started shell made of its hosted servers.
// mcpshell connects them before it serves, and says so on stderr; the wait is
// for the reader to catch up with "serving on stdio".
func (h *Harness) readHosted() {
	h.srvMu.Lock()
	hosted := h.hostedSpecs()
	h.srvMu.Unlock()
	if len(hosted) == 0 {
		h.srvMu.Lock()
		h.hosted = nil
		h.srvMu.Unlock()
		return
	}
	tail := ""
	for deadline := time.Now().Add(time.Second); ; {
		tail = h.mgr.ServerStderr(ServerShell)
		if strings.Contains(tail, "serving on stdio") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := parseHosted(tail, hosted)
	h.srvMu.Lock()
	h.hosted = got
	h.srvMu.Unlock()
}

// hostedNamespaces are the namespaces the model can call in the running
// shell: every hosted server that did not visibly fail. Empty when the shell
// is down.
func (h *Harness) hostedNamespaces() []string {
	if !h.mgr.ServerStarted(ServerShell) {
		return nil
	}
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	var out []string
	for _, s := range h.hostedSpecs() {
		if st, ok := h.hosted[s.ID]; ok && !st.Connected {
			continue
		}
		out = append(out, ShellNamespace(s.ID))
	}
	return out
}

// restartShell bounces a running shell so it picks up the hosted set. A
// stopped shell is left stopped: it reads the set when it next starts.
func (h *Harness) restartShell(ctx context.Context) (bool, error) {
	h.srvMu.Lock()
	sh, ok := h.spec(ServerShell)
	h.srvMu.Unlock()
	if !ok || !h.mgr.ServerStarted(ServerShell) {
		return false, nil
	}
	h.mgr.StopServer(ServerShell)
	return true, h.startServer(ctx, sh)
}

// MoveServer is SetServerVia that also reports whether the shell restarted:
// it does when the hosted set changed while it runs.
func (h *Harness) MoveServer(ctx context.Context, id, scope, via string) (bool, error) {
	hosted := func() string {
		h.srvMu.Lock()
		defer h.srvMu.Unlock()
		return strings.Join(ServerIDs(h.hostedSpecs()), ",")
	}
	before := hosted()
	if _, err := h.SetServerVia(ctx, id, scope, via); err != nil {
		return false, err
	}
	return h.mgr.ServerStarted(ServerShell) && hosted() != before, nil
}
