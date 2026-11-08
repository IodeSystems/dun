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
	// Recommended v0.7.0 (d14b745, 2026-10-29), tarballs in deps.json: on
	// the 401 re-run only, auth_cmd gets MCPSHELL_AUTH_RETRY=1 and
	// MCPSHELL_AUTH_REJECTED=<the refused token>, which `dun oauth token`
	// reads so dund refreshes a refused token (oauthbroker.go); regexp2
	// v2.12.0. Recommended v0.7.1 (185037c, 2026-10-29): reading a let from
	// an earlier eval says a let lasts one eval (it was "Unknown command",
	// or the builtin of that name); 1/0 shows Infinity. Recommended v0.8.0
	// (2026-11-04), tarballs in deps.json: JS semantics where it differed —
	// replace with a string or a regex without g changes the first match
	// (replaceAll all), split() with no separator is [input], reduce with no
	// initial value starts from the first element, Math.max()/min() with no
	// arguments are -Infinity/Infinity; rest params bind every argument;
	// slice(0, -1) and a stray break/continue no longer end the process. New
	// syntax: ??= ||= &&=, labeled loops, numeric separators, more methods.
	// Recommended v0.8.1 (2026-11-04), tarballs in deps.json: a Go panic in
	// a command is an "internal error" eval result, not the end of the MCP
	// server; the script, its vars and the stack go to stderr between
	// "--- script ---" and "--- end ---".
	// Recommended v0.9.0 (2026-11-06), tarballs in deps.json: async functions
	// and Promise methods on settled values; an async call returns a
	// rejection that raises in operators, conditions, typeof and ??, with its
	// call site; catch gets the bare message.
	// Recommended v0.10.0 (2026-11-07), tarballs in deps.json: JS bitwise
	// semantics (ToInt32/ToUint32 within ±2^53, exact above, where they
	// saturated to wrong results with no error: Terminal-Bench feal XORed
	// 64-bit values to all zeros), BigInt (BigInt(), 123n, asUintN/asIntN),
	// ^ is XOR and | is OR (|. and |: stay aliases), and x |> Number.
	// Recommended v0.11.0 (2026-11-08, 75b6c0a), tarballs in deps.json: the JS
	// object model (class, this, new, instanceof, Map/Set, Error subclasses),
	// solver sensitivity/explain, cp, route, num, geo, graph algorithms.
	// Breaking for scripts, not for dun's launch: extra arguments to a command
	// are an error; class/extends/super/this/instanceof are reserved; throw
	// keeps a non-string value. darwin_arm64 run on a Mac (2026-11-08).
	{Name: "mcpshell", Module: "github.com/iodesystems/mcpshell/cmd/mcpshell",
		Min: "v0.2.0", Recommended: "v0.11.0"},
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
	// through `lsp add` (servers.yaml). Recommended v0.4.1 (cdeeb2a,
	// 2026-11-03), tarballs in deps.json: child LSPs run at nice +10;
	// clangd -j is capped at half the cores (at most 8).
	{Name: "poly-lsp-mcp", Module: "github.com/iodesystems/lsp-mcp",
		Min: "v0.0.0-20261024175639-46caee4f6393", Recommended: "v0.4.1"},
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
	// Recommended v0.4.0 (9c539ee, 2026-10-31), tarballs in deps.json: never
	// open (a token or 401), keys (/rag login, a key per session, a child key
	// per session branch: dun/raglitauth.go), ingest --identity, and key mint
	// --json. A plain name it lacks is a 404, so all-off is the glob <own>:-*
	// (dun/attach.go). --embedded, --db and --embed are gone; ingest --now is
	// an alias of --wait. Older raglits still work: dun probes `help key`.
	{Name: "raglit", Module: "github.com/iodesystems/raglit/cmd/raglit",
		Min: "v0.0.0-20261024184452-5d24299365a3", Recommended: "v0.4.0"},
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
