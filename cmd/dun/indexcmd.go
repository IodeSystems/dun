package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/iodesystems/dun"
)

// /poly-lsp exclude <dir> and /poly-lsp include <dir>: what the code
// server's index leaves out, for this project (dun/indexstate.go). The
// same list dun writes when an index stops at its file limit, so include
// is the undo of what dun decided, and exclude the redo of what it offered.

// excludeVerb reports whether /<slash> <verb> is an exclude command: the
// code server's, whose index takes --exclude.
func excludeVerb(id, verb string) bool {
	return id == dun.ServerCode && (verb == "exclude" || verb == "include")
}

// excludeHelp is exclude's and include's lines in the palette and in bare
// /poly-lsp.
var excludeHelp = []string{
	"exclude", "DIR — leave a directory (relative to the workspace) out of the code index, for this project; the server restarts. The repository is not changed",
	"include", "DIR — put an excluded directory back in the code index (the undo of exclude, and of what dun excluded at the file limit)",
}

// runExcludeCmd applies /<slash> exclude|include <dir>.
func runExcludeCmd(ctx context.Context, h *dun.Harness, id, verb, dir string) string {
	slash := id
	if st, ok := stateOf(h, id); ok && slashOf(st) != "" {
		slash = slashOf(st)
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "usage: /" + slash + " " + verb + " <dir> — " + excludesNow(h, id)
	}
	on := verb == "exclude"
	path, changed, err := h.SetIndexExclude(ctx, id, dir, on)
	if err != nil && path == "" {
		return slash + ": " + oneLine(err.Error())
	}
	d := strings.TrimSuffix(dir, "/")
	var msg string
	switch {
	case !changed && on:
		msg = fmt.Sprintf("%s: %s is already left out of the index", slash, d)
	case !changed:
		msg = fmt.Sprintf("%s: %s is not excluded — %s", slash, d, excludesNow(h, id))
	case on:
		msg = fmt.Sprintf("%s: %s left out of the index, for this project (saved to %s) · undo: /%s include %s", slash, d, path, slash, d)
	default:
		msg = fmt.Sprintf("%s: %s back in the index, for this project (saved to %s) · undo: /%s exclude %s", slash, d, path, slash, d)
	}
	if err != nil {
		msg += "\nit did not restart: " + oneLine(err.Error())
	}
	return msg
}

// excludesNow is what id's index leaves out now, as a phrase.
func excludesNow(h *dun.Harness, id string) string {
	ex := h.IndexExcludes(id)
	if len(ex) == 0 {
		return "nothing is excluded"
	}
	return "excluded: " + strings.Join(ex, ", ")
}

// indexStatus is bare /poly-lsp's lines on the index: its state and what it
// leaves out, or "" for a server with neither.
func indexStatus(st dun.ServerState, slash string) string {
	var lines []string
	switch {
	case !st.Running:
	case st.Index == dun.IndexBuilding:
		lines = append(lines, fmt.Sprintf("index: building · %ds", int(st.IndexSeconds+0.5)))
	case st.Index == dun.IndexReady:
		lines = append(lines, fmt.Sprintf("index: ready · %s files · %.1fs", dun.Grouped(st.IndexFiles), st.IndexSeconds))
	case st.Index == dun.IndexFailed:
		lines = append(lines, "index: failed — "+oneLine(st.IndexErr))
	}
	if len(st.Exclude) > 0 {
		lines = append(lines, "excluded from the index (this project): "+strings.Join(st.Exclude, ", ")+
			" · /"+slash+" include <dir> takes one back")
	}
	return strings.Join(lines, "\n")
}
