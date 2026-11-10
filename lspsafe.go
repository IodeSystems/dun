package dun

import (
	"cmp"
	"strconv"
)

// Child-LSP flags for poly-lsp-mcp (2026-11-10).
//
// dun passes --lsp-roots single (one child per language, at the root; also
// poly-lsp-mcp's default) to a build whose `mcp --help` lists the flag.
//
// Until poly-lsp-mcp v0.5.1, dun also passed --lsp-max-rss-mb 2048 and
// --lsp-idle-stop 5m, because a child could grow to 6 GiB and idle for 15
// minutes (a Mac analyzing a directory of clones went down twice). v0.5.1
// bounds the whole daemon by default (4 children, an RSS budget, 10 min idle),
// and those two flags configure only the daemon's own fleet: the proxy
// ignores them with --daemon, and without --daemon they force in-process
// mode. dun passes neither by default.
//
// The server spec's LSPMaxRSSMB and LSPIdleStop, which the person writes in a
// config file, are still passed, without --daemon only. LSPRoots replaces
// "single". dun writes none of them.

const lspSafeRoots = "single"

// lspSafeArgs are the lifecycle flags s gets, each to a build that takes it
// (sup).
func lspSafeArgs(s Server, sup IndexSupport, daemon bool) []string {
	var out []string
	if sup.Roots {
		out = append(out, "--lsp-roots", cmp.Or(s.LSPRoots, lspSafeRoots))
	}
	if daemon {
		return out
	}
	if sup.MaxRSS && s.LSPMaxRSSMB != nil {
		out = append(out, "--lsp-max-rss-mb", strconv.Itoa(*s.LSPMaxRSSMB))
	}
	if sup.IdleStop && s.LSPIdleStop != "" {
		out = append(out, "--lsp-idle-stop", s.LSPIdleStop)
	}
	return out
}
