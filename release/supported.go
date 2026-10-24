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
	// 90cda54 (main, 2026-10-23): MCPSHELL_TMP_OVERFLOW_DIR, which dun sets
	// so over-limit output is saved to a file.
	{Name: "mcpshell", Module: "github.com/iodesystems/mcpshell/cmd/mcpshell",
		Min: "v0.2.0", Recommended: "v0.4.2-0.20261023182925-90cda545a60e"},
	// Min 46caee4 (main, 2026-10-24): the first pushed commit with the index
	// protocol (--exclude, --index-budget), doctor --json, and the pure-Go
	// tree-sitter, so go install needs Go alone. Recommended 9e227d7: git
	// decides the file set, leftOutBy, "not a project", the doctor update
	// block, and the daemon at parity with stdio. No tag yet.
	{Name: "poly-lsp-mcp", Module: "github.com/iodesystems/poly-lsp-mcp",
		Min: "v0.0.0-20261024175639-46caee4f6393", Recommended: "v0.0.0-20261024183910-9e227d797831"},
	// Min 5d24299 (main, 2026-10-24): the raglit/setup notification, doctor
	// --json and provider keys by api_key_file, which /raglit setup and
	// doctor read. Recommended v0.1.0 (a32c5db): raglit's first release, its
	// tarballs in deps.json.
	{Name: "raglit", Module: "github.com/iodesystems/raglit/cmd/raglit",
		Min: "v0.0.0-20261024184452-5d24299365a3", Recommended: "v0.1.0"},
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
