package dun

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Server is one MCP tool server dun spawns.
type Server struct {
	ID      string
	Command string
	Args    []string
	// URL and Headers make it a remote server (Streamable HTTP); Command and
	// Args are then unused. Headers carry the resolved bearer token.
	URL     string
	Headers map[string]string
	// Env entries ("KEY=value") for the spawned server — a per-machine DSN or
	// endpoint that cannot be baked into a committed config.
	Env []string
	// Timeout in seconds for tool discovery; 0 uses dun's default.
	Timeout int
	// Autostart spawns this server when the session starts. Off means the
	// server is available but idle until asked for (/lsp on, /rag on).
	Autostart bool
	// Disable is the GLOBAL deny-list of tool names to hide from the model.
	// When empty, all tools discovered from this server are passed through.
	Disable []string
	// ProjectDisable is a project file's deny-list (nil: none), which used
	// to replace Disable whole and still does; ProjectTools and SessionTools
	// turn single tools on or off at those scopes. See Server.toolState.
	ProjectDisable []string
	ProjectTools   map[string]bool
	SessionTools   map[string]bool
	// Via is "" (dun connects to it) or ViaShell (mcpshell hosts it and dun
	// does not start it). See ServerSpec.Via.
	Via string
	// Instance is how many connections it gets, resolved: InstanceShared,
	// InstanceSession or InstanceAgent; a built-in's fixed one
	// (builtinInstances). See ServerSpec.Instance.
	Instance string
	// Scopes is where Autostart, Via and Instance came from (mcpscope.go).
	Scopes ServerScopes
	// Shell is mcpshell's settings when it hosts the server
	// (ServerSpec.Shell).
	Shell *ShellOptions
	// Bearer is the token as configured, "$NAME" unexpanded, so a
	// shell-hosted server can read the variable when it connects instead of
	// dun writing the token into a file. Headers carries the resolved value.
	Bearer string
	// OAuth, when set, logs in to the server and sends the token it gets
	// (mcpoauth.go) instead of a fixed Bearer.
	OAuth *OAuthSpec
	// Attach and Detach are the indexes switched on and off, the nearest
	// scope deciding each (attach.go).
	Attach, Detach []string
	// Exclude is the directories left out of its index (ServerSpec.Exclude),
	// IndexBudget its index startup budget (ServerSpec.IndexBudget).
	Exclude     []string
	IndexBudget string
	// IndexAnyway indexes a root that is not a project (ServerSpec.IndexAnyway).
	IndexAnyway bool
	// Wake: its messages may start a turn on an idle session (ServerSpec.Wake).
	Wake bool
}

// raglitProject is the CORPUS: the repository, shared by every worktree of it.
//
// It must resolve to the main repo even when the session runs in a worktree, or
// each worktree gets its own corpus and re-embeds everything the others already
// paid for — the exact rework the corpus exists to kill. `--show-toplevel`
// answers the WORKTREE inside a worktree, so the repository is found through
// `--git-common-dir`, whose parent is the main checkout.
//
// A checkout that has been `raglit init`ed carries its own project name; that is
// the human's declared name and it wins, because splitting one repo across two
// corpora is the failure this exists to avoid.
func raglitProject(workspace string) string {
	root := repoIdentity(workspace)
	if name := raglitConfigProject(root); name != "" {
		return name
	}
	return normalizeProject(filepath.Base(strings.TrimRight(root, string(os.PathSeparator))))
}

// raglitIndex is the MEMBERSHIP set: this directory on this branch.
//
// Two worktrees of one repo on two branches get two indexes, so a search returns
// the files of the branch being worked on rather than a sibling's — dun's plan
// records the day a workspace-root ingest indexed 16 worktree copies and every
// search came back entirely stale duplicates. The corpus is what stops that
// isolation costing 16x the embeddings.
func raglitIndex(workspace string) string {
	dir := filepath.Base(strings.TrimRight(workspace, string(os.PathSeparator)))
	if b := gitLine(workspace, "rev-parse", "--abbrev-ref", "HEAD"); b != "" && b != "HEAD" {
		return normalizeProject(dir + "-" + b)
	}
	return normalizeProject(dir)
}

// repoIdentity is the main checkout of whatever repo workspace belongs to, or
// workspace itself when it is not a repo at all.
func repoIdentity(workspace string) string {
	common := gitLine(workspace, "rev-parse", "--git-common-dir")
	if common == "" {
		return workspace
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workspace, common)
	}
	if abs, err := filepath.Abs(filepath.Dir(common)); err == nil {
		return abs
	}
	return workspace
}

// gitLine runs a git command in dir and returns its single-line output, or "".
func gitLine(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// raglitConfigProject reads the project name a `raglit init` wrote next to this
// checkout, if there is one. Best-effort: any failure falls back to the
// directory name rather than refusing to start the docs server.
func raglitConfigProject(workspace string) string {
	data, err := os.ReadFile(filepath.Join(workspace, ".raglit", "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Project string `json:"project"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return normalizeProject(cfg.Project)
}

// normalizeProject mirrors raglit's own index-name rule ([a-z0-9_-], empty →
// "default"), so dun cannot hand it a name it will reject or sanitize
// differently.
func normalizeProject(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else if r == ' ' || r == '.' || r == '/' {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

// DefaultServers points the three tool servers at a workspace directory (later a
// git worktree, and later still `docker exec` into a container).
//
// Only shell autostarts. The other two are opt-in because each costs real
// startup time and neither is universally wanted: poly-lsp-mcp indexes the
// repo, and raglit needs the workspace ingested before it answers anything.
// A session that is going to run a couple of shell commands should not pay for
// either, and — more to the point — a machine missing one of those binaries
// should still get a working dun. Persist a preference with /rag auto or
// /lsp auto (see SetServerOn).
//
// poly-lsp and raglit are bundled extensions: their servers are their
// manifests' (examples/extensions/, extension.go).
func DefaultServers(workspace, raglitHome string) []Server {
	code, _ := bundledExt(ServerCode)
	docs, _ := bundledExt(ServerDocs)
	return []Server{
		bundledServer(code, workspace, raglitHome),
		{ID: "shell", Command: "mcpshell", Args: []string{"mcp", "--files-dir", workspace}, Autostart: true},
		// raglit is the shared daemon, namespaced by PROJECT — not an
		// --embedded index in a per-session temp dir ({{project}} and
		// {{index}} in its manifest).
		//
		// The temp-dir form was wrong twice over. It re-embedded the entire
		// workspace on every session and threw the result away on exit, and it
		// opted out of the daemon that exists for exactly dun's shape: raglit's
		// own comment says "N Claude sessions are N thin clients to ONE daemon
		// (single writer + worker pool + LLM caller), not N processes fighting
		// over the same index" — and this repo routinely runs seven at once.
		//
		// It also made durable memory impossible: anything written to a home
		// that is deleted on exit cannot outlive the session that wrote it.
		bundledServer(docs, workspace, raglitHome),
	}
}

// bundledServer is a bundled extension's server for workspace, its
// placeholders expanded.
func bundledServer(e Extension, workspace, raglitHome string) Server {
	s := e.server()
	s.Args = expandPlaceholders(s.Args, workspace, raglitHome)
	s.Env = expandPlaceholders(s.Env, workspace, raglitHome)
	return s
}

// Server ids dun knows by name. The id of poly-lsp-mcp and of raglit is the
// name of the thing and of its command: /lsp, /rag. Their tools are
// still unprefixed (see modelToolName). Config written before the rename says
// "code" and "docs"; canonicalServerID reads those as the new ids.
const (
	ServerCode  = "poly-lsp" // poly-lsp-mcp — /lsp
	ServerShell = "shell"    // mcpshell
	ServerDocs  = "raglit"   // raglit — /rag
)

// builtinInstance is a built-in server's fixed instance and why it is
// fixed: the instance switch (ServerSpec.Instance) is refused for these, and
// the /mcp table shows this value, marked built-in.
type builtinInstance struct{ instance, why string }

// builtinInstances are the built-in servers' fixed instances, the one place
// they are declared (mcpscope.go resolve and SetServerInstance read it).
var builtinInstances = map[string]builtinInstance{
	// mcpshell holds the session's eval state (exported variables, its
	// hosted servers) and the engine starts one per session.
	ServerShell: {InstanceSession, "it holds this session's eval state"},
	// The engine starts a poly-lsp-mcp proxy per session; with --daemon the
	// proxies share one daemon, which holds one index and one LSP fleet per
	// root ({{workspace}}).
	ServerCode: {InstanceWorkspace, "its daemon keeps one index per workspace"},
	// `raglit serve` is a thin per-session client of the one per-user
	// raglit daemon, which holds every project's index.
	ServerDocs: {InstanceShared, "every session's raglit is a client of one raglit daemon"},
}

// BuiltinInstance is id's fixed instance and why, ok=false for a server
// that is not built in.
func BuiltinInstance(id string) (instance, why string, ok bool) {
	b, ok := builtinInstances[canonicalServerID(id)]
	return b.instance, b.why, ok
}

// legacyServerIDs are the ids the two optional servers had before they were
// named for what they are. Files and sidecars may still hold them.
var legacyServerIDs = map[string]string{"code": ServerCode, "docs": ServerDocs}

// canonicalServerID is id with a pre-rename id read as its current one.
func canonicalServerID(id string) string {
	if n, ok := legacyServerIDs[id]; ok {
		return n
	}
	return id
}
