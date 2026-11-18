package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// File access asks in the turn (USER 2026-11-18, docs/decisions.md). A file
// call for a path outside the workspace that no scope opens, or for one open
// for reading only when the call writes, pauses the turn and asks the
// person: yes for this session, this project or everywhere, or no — in
// their own words, which the model gets as the reason. A yes opens the path
// the call named (the file, or the directory) at that scope, for writing
// when the call writes, and unindexed: raglit watching a directory is not
// what a file call asked for (/file index turns it on). The call then runs
// once and its result is the answer. Before, the model was told to ask for
// /file add, and in 17 sessions it edited through exec instead.
//
// Nobody to ask (a sub-agent, --non-interactive: no Ask) and the refusal is
// as before. In a round dun added (autoRound) the refusal also says to
// block the ask with the question, as ask_user's does.

const (
	accessSession = "yes, for this session"
	accessProject = "yes, for this project"
	accessGlobal  = "yes, everywhere"
	accessNo      = "no"
)

// accessScope reads the person's answer: a scope, or "" and their words
// when they said no (reason "" for a bare no).
func accessScope(ans string) (scope, reason string) {
	a := strings.TrimSpace(ans)
	switch strings.ToLower(a) {
	case accessSession, "yes", "y":
		return mcpserver.ScopeSession, ""
	case accessProject:
		return mcpserver.ScopeProject, ""
	case accessGlobal:
		return mcpserver.ScopeGlobal, ""
	case accessNo, "n", "":
		return "", ""
	}
	return "", a
}

// outsideHint is the answer to a file call for a path outside the
// workspace that dun has not been given (abs: outsidePath's): the model is
// to ask the person for it, with the command.
func outsideHint(ts *filetools.Toolset, name, abs string) string {
	if abs == "" {
		return ""
	}
	return "ERROR: " + outsideWhat(ts, abs) + " Ask the user to add it: " + addCommand(ts, name, abs) + ". Do not work around this with exec."
}

func outsideWhat(ts *filetools.Toolset, abs string) string {
	return fmt.Sprintf("%s is outside this workspace (%s), and the user has not added it.", abs, ts.Root())
}

// addCommand is the /file add that opens abs for the call, and for a file
// its directory as the other choice. A write or an edit needs the path
// open for writing: the command says so, or the person adds it read only
// and the call is refused again.
func addCommand(ts *filetools.Toolset, name, abs string) string {
	write := ""
	if writesFiles(ts, name) {
		write = " --write"
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return "/file add " + abs + write
	}
	return fmt.Sprintf("/file add %s%s (or /file add %s%s for its directory)", abs, write, filepath.Dir(abs), write)
}

// writesFiles reports whether the file tool called name changes files.
func writesFiles(ts *filetools.Toolset, name string) bool {
	switch ts.Bare(name) {
	case filetools.Edit, filetools.Write, filetools.Delete, filetools.Move:
		return true
	}
	return false
}

// outsidePath is the real absolute path a file call names when it lies
// outside the working tree — absolute, climbing out with `..`, or through
// a symlink whose target is outside — and "" when it does not, or names no
// path.
func outsidePath(ts *filetools.Toolset, args string) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	p := strings.TrimSpace(a.Path)
	if p == "" {
		return ""
	}
	if _, err := filetools.Under(ts.Root(), p); !errors.Is(err, filetools.ErrEscape) {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ts.Root(), p)
	}
	real := resolveExisting(filepath.Clean(p))
	if _, err := filetools.Under(ts.Root(), real); err == nil {
		return "" // the tree, named through a link to it
	}
	return real
}

// withPath is args with its path replaced by p.
func withPath(args, p string) string {
	var a map[string]any
	if json.Unmarshal([]byte(args), &a) != nil {
		return args
	}
	a["path"] = p
	b, _ := json.Marshal(a)
	return string(b)
}

// coveringEntry is the open path that grants abs: abs itself, or the
// nearest open directory above it; nil when none does.
func (h *Harness) coveringEntry(abs string) *AddedPath {
	var best *AddedPath
	for _, a := range h.AddedEntries() {
		if !a.On {
			continue
		}
		if a.Path == abs {
			return &a
		}
		if a.Dir && strings.HasPrefix(abs, a.Path+string(filepath.Separator)) && (best == nil || len(a.Path) > len(best.Path)) {
			best = &a
		}
	}
	return best
}

// askFunc is the session's way to ask the person; nil when there is none.
func (h *Harness) askFunc() AskFunc {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.cfg.Ask
}

// fileAccess runs before a file call is routed. It returns the arguments to
// route — an outside path resolved, so a `..` or a link reaches the grant
// that holds it — and, when the call is not to run, its answer.
func (h *Harness) fileAccess(ctx context.Context, ts *filetools.Toolset, tmp []*filetools.Toolset, name, args string) (string, string) {
	if tmpReader(ts, tmp, name, args) != nil {
		return args, ""
	}
	abs := outsidePath(ts, args)
	if abs == "" {
		return args, ""
	}
	args = withPath(args, abs)
	writes := writesFiles(ts, name)
	e := h.coveringEntry(abs)
	if e != nil && (e.Write || !writes) {
		return args, ""
	}
	ask := h.askFunc()
	if ask == nil || h.isChild() {
		return args, "" // nobody to ask: the routing refuses with the command
	}
	what, cmd, target := outsideWhat(ts, abs), addCommand(ts, name, abs), abs
	if e != nil {
		what = fmt.Sprintf("%s is open to dun for reading only (%s).", abs, e.Path)
		cmd, target = "/file mode "+e.Path+" write", e.Path
	}
	if h.autoRound() {
		return args, "ERROR: " + what + " dun started this round, not the user, so no one may be there to answer, " +
			"and the file tools did not ask. Do not wait for an answer and do not work around this with exec. " +
			"The question for the user: " + cmd + ". " + askBlockHow
	}
	kind := "the file"
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		kind = "the directory, and everything under it,"
	}
	var q string
	if e != nil {
		q = fmt.Sprintf("%s wants to change %s, which is open to dun for reading only (%s). Let dun's file tools change %s %s?",
			name, abs, e.Path, kind, target)
	} else {
		q = fmt.Sprintf("%s wants to %s %s, outside this workspace (%s). Open %s to dun's file tools, %s?",
			name, map[bool]string{true: "change", false: "read"}[writes], abs, ts.Root(), kind,
			map[bool]string{true: "to read and write", false: "to read only"}[writes])
	}
	q += " Or answer in your own words: the model gets them as the reason."
	ans, err := ask(ctx, q, []string{accessSession, accessProject, accessGlobal, accessNo}, false, []string{accessSession})
	if err != nil {
		return args, "ERROR: " + what + " The user was asked and gave no answer (" + err.Error() + "). Do not work around this with exec."
	}
	scope, reason := accessScope(ans)
	if scope == "" {
		out := "ERROR: the user did not open " + target + " to dun's file tools"
		if reason != "" {
			out += ". They said: " + reason
		}
		return args, out + ". Do not work around this with exec."
	}
	if e != nil {
		_, err = h.SetPathMode(e.Path, scope, "write")
	} else {
		_, err = h.AddPath(abs, scope, PathOptions{Write: writes, NoIndex: true})
	}
	if err != nil {
		return args, "ERROR: the user said " + ans + ", and opening " + target + " failed: " + err.Error()
	}
	return args, ""
}
