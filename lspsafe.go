package dun

import "cmp"

// Child-LSP flags for poly-lsp-mcp (2026-11-10).
//
// dun passes --lsp-roots single (one child per language, at the root; also
// poly-lsp-mcp's default) to a build whose `mcp --help` lists the flag.
//
// Until poly-lsp-mcp v0.5.1, dun also passed --lsp-max-rss-mb 2048 and
// --lsp-idle-stop 5m, because a child could grow to 6 GiB and idle for 15
// minutes (a Mac analyzing a directory of clones went down twice). v0.5.1
// bounds the whole daemon by default (4 children, an RSS budget, 10 min idle),
// so dun passes neither, and the server-spec settings lsp_max_rss_mb and
// lsp_idle_stop are removed. LSPRoots replaces "single". dun writes none.

const lspSafeRoots = "single"

// lspSafeArgs are the lifecycle flags s gets, each to a build that takes it
// (sup).
func lspSafeArgs(s Server, sup IndexSupport) []string {
	if !sup.Roots {
		return nil
	}
	return []string{"--lsp-roots", cmp.Or(s.LSPRoots, lspSafeRoots)}
}
