package dun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A root that is not a project (plan item 50).
//
// poly-lsp-mcp refuses a root that is the home directory, /, or a directory
// of three or more clones outside any repository: the bring-up fails at once
// with reason "not a project", the repositories it found (repos, at most 20,
// of reposTotal) and the remedy — --root <one repo>, or --index-anyway.
//
// Indexing such a root anyway is the person's choice and only theirs
// (2026-11-09): an index of a directory of clones took a machine down
// twice. dun says once per session that the code server is off, which
// repositories to start dun in, and that /lsp index-anyway on indexes the
// root regardless. It asks no question: an answer to Config.Ask can come
// from a model (dun mcp's dun_answer relays the session's questions to the
// agent driving it; a headless run reads its answer from a pipe), so no
// answer to it can set ServerSpec.IndexAnyway. Only /lsp index-anyway (a
// server command, from a client: the TUI's slash, dund's token-gated
// control API) and a config file the person writes set it; no tool does.
//
// The line names the slash only to a binary whose `mcp --help` lists
// --index-anyway (IndexProtocol). The rest of dun runs either way.

// notProjectRepoLines is how many repositories the hint line names.
const notProjectRepoLines = 5

// notProjectLine handles a not-a-project failure from id and returns the line
// for the person (and, as an aside, the model), once per session. It never
// sets index-anyway: the answer is always "not now".
func (h *Harness) notProjectLine(id string, n indexNote) string {
	h.srvMu.Lock()
	asked := h.indexAsked[id]
	if h.indexAsked == nil {
		h.indexAsked = map[string]bool{}
	}
	h.indexAsked[id] = true
	h.srvMu.Unlock()
	if asked {
		return "" // said once; no second nagging
	}
	s, err := h.knownServer(id)
	if err != nil {
		return ""
	}
	dir := h.tildePath(h.cfg.Workspace)
	hint := notProjectHint(n)
	slash := h.slashFor(id)
	sup := IndexSupport{}
	if command, err := FindCommand(s.Command); err == nil {
		sup, _, _ = IndexProtocol(command)
	}
	if !sup.Anyway || s.IndexAnyway {
		// Nothing to offer: the binary cannot, or already did and still
		// refused.
		return fmt.Sprintf("%s: %s is not a project — the code tools need one. %s", id, dir, hint)
	}
	line := fmt.Sprintf("%s: off for this session — %s is not a project%s; the rest of dun works.", id, dir, reposInside(n))
	if hint != "" {
		line += "\n" + hint
	}
	return line + fmt.Sprintf("\n/%s index-anyway on indexes %s regardless (the person's choice: dun never sets it).", slash, dir)
}

// reposInside is " (12 git repos inside)", or "" when none were found.
func reposInside(n indexNote) string {
	total := max(n.ReposTotal, len(n.Repos))
	switch total {
	case 0:
		return ""
	case 1:
		return " (1 git repo inside)"
	}
	return fmt.Sprintf(" (%d git repos inside)", total)
}

// notProjectHint is the line naming up to notProjectRepoLines repositories
// to start dun in, "" when none were found.
func notProjectHint(n indexNote) string {
	if len(n.Repos) == 0 {
		return ""
	}
	var cmds []string
	for _, r := range n.Repos[:min(len(n.Repos), notProjectRepoLines)] {
		cmds = append(cmds, "cd "+r+" && dun")
	}
	more := ""
	if total := max(n.ReposTotal, len(n.Repos)); total > len(cmds) {
		more = fmt.Sprintf(" · and %d more", total-len(cmds))
	}
	return "start dun in a repository instead: " + strings.Join(cmds, " · ") + more
}

// tildePath is p with the home directory written "~".
func (h *Harness) tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}

// IndexAnyway is whether id indexes a root that is not a project, for this
// project.
func (h *Harness) IndexAnyway(id string) bool {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	s, _ := h.spec(id)
	return s.IndexAnyway
}

// SetIndexAnyway lets id index a root that is not a project (on) or stops it
// (off), for this project; a running server restarts. It reports the file
// written and whether anything changed. A binary that does not take
// --index-anyway is an error to turn it on.
func (h *Harness) SetIndexAnyway(ctx context.Context, id string, on bool) (string, bool, error) {
	s, err := h.knownServer(id)
	if err != nil {
		return "", false, err
	}
	if s.IndexAnyway == on {
		return "", false, nil
	}
	if on {
		command, err := FindCommand(s.Command)
		if err != nil {
			return "", false, err
		}
		if sup, _, err := IndexProtocol(command); err != nil {
			return "", false, err
		} else if !sup.Anyway {
			return "", false, fmt.Errorf("%s is an older build: it takes no --index-anyway; update it, or start dun in a repository", command)
		}
	}
	p, err := h.editIndexSpec(ctx, id, true, func(s *ServerSpec) { s.IndexAnyway = on })
	return p, err == nil, err
}
