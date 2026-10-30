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
// dun asks once per session, through the same question the model's ask_user
// puts (Config.Ask: the picker in the TUI, an `ask` event for a client):
//
//   - "index anyway": ServerSpec.IndexAnyway is saved for this project and
//     the server restarts with --index-anyway. Undo: /lsp index-anyway
//     off.
//   - "not now": the code server stays failed for the session and dun does
//     not ask again; the line says which repository to start dun in.
//
// Only a binary whose `mcp --help` lists --index-anyway is offered the first
// answer (IndexProtocol); for an older one, or with no Config.Ask (a client
// that cannot ask), dun says the same in one line and asks nothing. The rest
// of dun runs either way.

// Not-a-project answers, as the question offers them.
const (
	answerIndexAnyway = "index anyway"
	answerNotNow      = "not now"
)

// notProjectRepoLines is how many repositories the hint line names.
const notProjectRepoLines = 5

// askNotProject handles a not-a-project failure from id and returns the line
// for the person (and, as an aside, the model). It blocks on the person's
// answer; afterIndex runs it off the client's read goroutine.
func (h *Harness) askNotProject(id string, n indexNote) string {
	h.srvMu.Lock()
	asked := h.indexAsked[id]
	if h.indexAsked == nil {
		h.indexAsked = map[string]bool{}
	}
	h.indexAsked[id] = true
	h.srvMu.Unlock()
	if asked {
		return "" // the person has been asked; no second nagging
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
	if !sup.Anyway || s.IndexAnyway || h.cfg.Ask == nil {
		// Nothing to offer: the binary cannot, or already did and still
		// refused; or there is no one to ask.
		line := fmt.Sprintf("%s: %s is not a project — the code tools need one. %s", id, dir, hint)
		if sup.Anyway && !s.IndexAnyway {
			line += fmt.Sprintf(" · /%s index-anyway on indexes it regardless", slash)
		}
		return line
	}
	q := fmt.Sprintf("dun is running in %s — not a project%s. Index all of it anyway, or start dun in one of them? (the code tools need a project)",
		dir, reposInside(n))
	if hint != "" {
		q += "\n" + hint
	}
	ans, err := h.cfg.Ask(context.Background(), q, []string{answerIndexAnyway, answerNotNow}, false, []string{answerNotNow})
	if err != nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(ans), answerIndexAnyway) {
		saved, err := h.editIndexSpec(context.Background(), id, false, func(s *ServerSpec) { s.IndexAnyway = true })
		if err != nil {
			return fmt.Sprintf("%s: would index %s anyway, but could not save that: %v", id, dir, err)
		}
		return fmt.Sprintf("%s: indexing %s anyway, for this project (saved to %s) · undo: /%s index-anyway off", id, dir, saved, slash)
	}
	line := fmt.Sprintf("%s: off for this session — %s is not a project; the rest of dun works.", id, dir)
	if hint != "" {
		line += "\n" + hint
	}
	return line + fmt.Sprintf("\n/%s index-anyway on indexes %s regardless.", slash, dir)
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
