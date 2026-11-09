package dun

import (
	"cmp"
	"strconv"
)

// Safe child-LSP limits for poly-lsp-mcp (2026-11-09).
//
// poly-lsp-mcp's own defaults let a child LSP (gopls, tsserver, pylsp) grow
// to 6 GiB and idle for 15 minutes; on a Mac analyzing a directory of
// clones, that took the machine down twice. dun passes tighter ones to a
// build whose `mcp --help` lists the flag (IndexProtocol):
//
//	--lsp-roots single      one child per language, at the root
//	--lsp-max-rss-mb 2048   a child over 2 GiB is stopped (it starts again on use)
//	--lsp-idle-stop 5m      a child idle 5 minutes is stopped
//
// Each one is overridden by the server spec (ServerSpec.LSPRoots,
// LSPMaxRSSMB, LSPIdleStop) in a config file the person writes; dun writes
// none of them.
//
// In daemon mode (--daemon) the proxy takes --lsp-roots with its open, but
// --lsp-max-rss-mb and --lsp-idle-stop configure the daemon's own child
// fleet: the proxy ignores them, and given without --daemon they make
// poly-lsp-mcp serve in-process (its inProcessOnlyFlags). So with --daemon
// dun passes --lsp-roots only, and the daemon's own limits apply.

// The safe defaults.
const (
	lspSafeRoots    = "single"
	lspSafeMaxRSSMB = 2048
	lspSafeIdleStop = "5m"
)

// lspSafeArgs are the lifecycle flags s gets: dun's safe values or the
// spec's, each to a build that takes it (sup); with daemon, --lsp-roots
// only.
func lspSafeArgs(s Server, sup IndexSupport, daemon bool) []string {
	var out []string
	if sup.Roots {
		out = append(out, "--lsp-roots", cmp.Or(s.LSPRoots, lspSafeRoots))
	}
	if daemon {
		return out
	}
	if sup.MaxRSS {
		mb := lspSafeMaxRSSMB
		if s.LSPMaxRSSMB != nil {
			mb = *s.LSPMaxRSSMB
		}
		out = append(out, "--lsp-max-rss-mb", strconv.Itoa(mb))
	}
	if sup.IdleStop {
		out = append(out, "--lsp-idle-stop", cmp.Or(s.LSPIdleStop, lspSafeIdleStop))
	}
	return out
}
