package release

// Supported is the tools dun runs and installs, in every build (not only
// release builds: these are versions, not download locations): the module
// `go install` builds each from, the oldest version this dun works with
// (Min), and the version it was tested with (Recommended), which `dun
// doctor --fix` installs when a tool is missing and updates to when one is
// behind. Owner, item 49: a release build installs from its release's
// manifest; any other build go-installs the recommended version.
//
// Hand-edited, with the dun change that needs the newer tool. A version is
// a module version the Go proxy resolves — a tag, or the pseudo-version of a
// pushed commit (`GOFLAGS=-mod=mod go list -m <module>@<commit>`), never
// "latest": the proxy caches @latest, and a cached one can be a build that
// no longer works (poly-lsp-mcp's 9f31d7e needs a C compiler). A release
// build's deps.json must name these same versions (supported_test.go).
var Supported = []Tool{
	// v0.2.0: the oldest tag with --files-dir and --mcp, which dun passes.
	// v0.5.1 (2026-10-24): MCPSHELL_TMP_OVERFLOW_DIR, which dun sets so
	// over-limit output is saved to a file; per-server unchecked globs, which
	// a server spec's "shell" block passes; and each hosted server's schema
	// and help sizes, which /mcp info's savings read. Its tarballs are in
	// deps.json. v0.6.0 (de7d047, 2026-10-25), tarballs in deps.json:
	// help("ns") lists every tool (up to 16 KB, so /mcp info's help cost
	// grows); a truncated value is kept as $rN; results may end with
	// "warning: line N: …" lines, which dun's eval renderer shows under the
	// value; round(2.5) = 3. No flag or env var changed. Recommended v0.6.1
	// (5ca5898, 2026-10-27), tarballs in deps.json: built -tags release, so
	// dun doctor calls it a release (v0.6.0's said "built from a checkout");
	// typeof of an undeclared name is "undefined"; golang.org/x/net v0.62.0.
	{Name: "mcpshell", Module: "github.com/iodesystems/mcpshell/cmd/mcpshell",
		Min: "v0.2.0", Recommended: "v0.6.1"},
	// Min 46caee4 (main, 2026-10-24): the first pushed commit with the index
	// protocol (--exclude, --index-budget), doctor --json, and the pure-Go
	// tree-sitter, so go install needs Go alone. v0.1.0 (6dab491), its first
	// release: git decides the file set, leftOutBy, "not a project", the
	// doctor update block, and the shared daemon dun runs it through (the
	// proxy keeps the hook, changed options rebuild a root, roots are
	// released after their last client, a root outside $HOME is admitted).
	// Recommended v0.2.0 (16f2a76, 2026-10-24), tarballs in deps.json: daemon
	// mode is the default (dun still passes --daemon; in-process-only flags
	// are ignored with a log line, and a daemon that cannot start falls back
	// to in-process), and --daemon-home / $POLY_LSP_HOME, which dun's tests
	// set. Recommended v0.3.1 (4f1fd74, 2026-10-27), tarballs in
	// deps.json: Swift (sourcekit-lsp); child LSPs found outside PATH (pnpm,
	// npm -g, volta, bun, go, cargo, homebrew); node_read/node_edit on own
	// files under --allow-path (default $TMPDIR, /tmp), parse-only;
	// --validate-wait; a clean edit by a server not trusted for clean
	// diagnostics (sourcekit-lsp, typescript) reports validated:false,
	// validationSkipped:"lsp-unconfirmed". MCP tool names unchanged.
	// Recommended v0.4.0 (80f6d1d, 2026-10-29), tarballs in deps.json: `lsp
	// list|add|remove` (the /lsp table), per-client --lsp-off / --lsp-on and
	// --format-on-edit / --no-format-on-edit; the shared daemon reads a
	// config only from an absolute --config, so custom servers reach it
	// through `lsp add` (servers.yaml).
	{Name: "poly-lsp-mcp", Module: "github.com/iodesystems/lsp-mcp",
		Min: "v0.0.0-20261024175639-46caee4f6393", Recommended: "v0.4.0"},
	// Min 5d24299 (main, 2026-10-24): the raglit/setup notification, doctor
	// --json and provider keys by api_key_file, which /rag setup and
	// doctor read. v0.1.0 (a32c5db): raglit's first release. v0.2.0 (574d91e,
	// 2026-10-25): no flag, MCP tool, notification or doctor --json change;
	// /api/settings/models, which /rag setup uses, is unchanged; a
	// Markdown file's first heading is its title on (re)ingest. Recommended
	// v0.2.1 (c4df764, 2026-10-26): its version string comes from the build
	// info (v0.2.0's said 0.1.0). Recommended v0.3.0 (e434555, 2026-10-27),
	// tarballs in deps.json: `index --list --json`, every project's indexes,
	// which bare /rag attach lists (dun/indexkind.go); ":<index>" reads a
	// global index. MCP tools, notifications and doctor --json unchanged;
	// search ranks per document and new files get smaller fragments.
	{Name: "raglit", Module: "github.com/iodesystems/raglit/cmd/raglit",
		Min: "v0.0.0-20261024184452-5d24299365a3", Recommended: "v0.3.0"},
}

// Tool is one Supported entry.
type Tool struct {
	Name        string // the command
	Module      string // the package go install builds (no @version)
	Min         string // the oldest version dun works with
	Recommended string // the version this dun was tested with
}

// SupportedTool is name's entry.
func SupportedTool(name string) (Tool, bool) {
	for _, t := range Supported {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
