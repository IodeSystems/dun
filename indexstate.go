package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// A server's index, after its handshake (plan item 46).
//
// poly-lsp-mcp used to build its index before it answered initialize: 52 s
// on dun's own tree with 48 worktrees in it, all of it "starting". A newer
// build answers at once and builds behind the handshake; a tool call waits
// for the build. When the build ends it says so, unprompted, as
// notifications/message from logger "poly-lsp-mcp/index":
//
//	{"state": "ready"|"failed", "ms": N, "index": {files, bytes, cached,
//	 wallMs, heaviest: [{dir, files, bytes, costMs, share,
//	 nestedCheckouts}], hint}, "error", "remedy", "limit",
//	 "large": {files, threshold, message}}
//
// "large" is on a ready build over --index-warn-files (default 10,000):
// built, but big. "limit" is on a build that stopped at the hard cap,
// --index-max-files (default 100,000). Past the cap the walk goes on
// counting, so heaviest is the whole post-mortem: where the files are.
//
// What dun does with it:
//
//   - building: the server is up but its index is not; Servers() says
//     Index "building" and for how long ("poly-lsp indexing 12s").
//   - ready: Index "ready". A large one gets one quiet line, once per
//     session: its size, heaviest, and the command that leaves the obvious
//     directories out — offered, not done. Otherwise a directory in
//     heaviest that holds other git checkouts gets one quiet line, with the
//     same command.
//   - failed at the limit: dun picks the obvious directories to leave out
//     (chooseExcludes), saves them in the project's server config
//     (ServerSpec.Exclude, $DUN_HOME/projects/<id>/dun.local.json) so a
//     later start does not hit the limit again, restarts the server with
//     --exclude for each, and says what it left out, why, and how to undo
//     it (/poly-lsp include <dir>). Once per session; a second failure is
//     reported, not retried. With nothing obvious it changes nothing and
//     reports the post-mortem and the remedy.
//
// The initialize result also says "index": {"state": "building"}, but
// agentkit's mcpmgr (v0.10.1) keeps only the capabilities of that result.
// So "building" comes from the binary instead: a poly-lsp-mcp whose
// `mcp --help` lists --index-max-files speaks this protocol (IndexProtocol).
// An older one builds before it answers (Starting covers it), sends no
// notification, and rejects --exclude; dun passes it no excludes and shows
// no index state — it behaves as before. Any server that sends a
// "<name>/index" notification gets the same state, whatever its binary.

// Index states, as ServerState.Index says them.
const (
	IndexBuilding = "building"
	IndexReady    = "ready"
	IndexFailed   = "failed"
)

// IndexReport is what an index build cost, by directory: poly-lsp-mcp's
// CostReport.
type IndexReport struct {
	Files    int        `json:"files"`
	Bytes    int64      `json:"bytes"`
	Cached   int        `json:"cached"`
	WallMs   int64      `json:"wallMs"`
	Heaviest []IndexDir `json:"heaviest"`
	Hint     string     `json:"hint,omitempty"`
}

// IndexDir is one directory's share of a build, its subdirectories in it.
// Dir is relative to the root and ends in "/".
type IndexDir struct {
	Dir             string  `json:"dir"`
	Files           int     `json:"files"`
	Bytes           int64   `json:"bytes"`
	CostMs          int64   `json:"costMs"`
	Share           float64 `json:"share"` // of the indexed bytes, 0–1
	NestedCheckouts int     `json:"nestedCheckouts,omitempty"`
}

// indexNote is one index notification's data.
type indexNote struct {
	State  string       `json:"state"`
	Ms     int64        `json:"ms"`
	Index  *IndexReport `json:"index,omitempty"`
	Error  string       `json:"error,omitempty"`
	Remedy string       `json:"remedy,omitempty"`
	Limit  int          `json:"limit,omitempty"`
	Large  *indexLarge  `json:"large,omitempty"`
}

// indexLarge is a ready build over --index-warn-files: the workspace is
// large, the index was built anyway.
type indexLarge struct {
	Files     int    `json:"files"`
	Threshold int    `json:"threshold"`
	Message   string `json:"message"`
}

// indexState is a server's index as dun last heard of it. Guarded by srvMu.
type indexState struct {
	state string
	since time.Time // building: when the server was spawned
	note  indexNote // ready or failed: what the server said
}

// indexLoggerSuffix marks a notifications/message logger as an index
// report: "poly-lsp-mcp/index".
const indexLoggerSuffix = "/index"

// parseIndexNote reads an index notification's params, ok=false when they
// are not one (another logger, or no state).
func parseIndexNote(params map[string]any) (indexNote, bool) {
	logger, _ := params["logger"].(string)
	if !strings.HasSuffix(logger, indexLoggerSuffix) {
		return indexNote{}, false
	}
	b, err := json.Marshal(params["data"])
	if err != nil {
		return indexNote{}, false
	}
	var n indexNote
	if json.Unmarshal(b, &n) != nil || (n.State != IndexReady && n.State != IndexFailed && n.State != IndexBuilding) {
		return indexNote{}, false
	}
	return n, true
}

// polyLSPCommand is the base name of the binary IndexProtocol knows how to
// ask.
const polyLSPCommand = "poly-lsp-mcp"

// asksIndex reports whether dun asks s's binary whether it speaks the index
// protocol: poly-lsp-mcp by name, or the code server.
func asksIndex(s Server) bool {
	return s.URL == "" && s.Command != "" && (filepath.Base(s.Command) == polyLSPCommand || s.ID == ServerCode)
}

// indexProbes caches IndexProtocol by the binary's path, size and mtime: a
// rebuilt binary is asked again, an unchanged one is not.
var indexProbes sync.Map // key → bool

// indexProbeTimeout bounds the `mcp --help` IndexProtocol runs.
const indexProbeTimeout = 5 * time.Second

// IndexProtocol reports whether the poly-lsp-mcp named by command (a path,
// or a name looked up on PATH) speaks the index protocol: its initialize
// answers before the index is built, the build's end is a notification,
// and it takes --exclude and --index-max-files. It runs `<command> mcp
// --help` — a read: LSP_REBUILD_DONE stops a dev build's self-update — and
// looks for --index-max-files. resolved is the binary that was asked.
func IndexProtocol(command string) (ok bool, resolved string, err error) {
	resolved, err = exec.LookPath(command)
	if err != nil {
		return false, "", err
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return false, resolved, err
	}
	key := fmt.Sprintf("%s|%d|%d", resolved, st.Size(), st.ModTime().UnixNano())
	if v, hit := indexProbes.Load(key); hit {
		return v.(bool), resolved, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), indexProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "mcp", "--help")
	cmd.Env = append(os.Environ(), "LSP_REBUILD_DONE=1")
	out, runErr := cmd.CombinedOutput()
	if len(out) == 0 && runErr != nil {
		return false, resolved, fmt.Errorf("%s mcp --help: %w", resolved, runErr)
	}
	ok = strings.Contains(string(out), "--index-max-files")
	indexProbes.Store(key, ok)
	return ok, resolved, nil
}

// speaksIndex reports whether s, as it is about to be spawned, builds its
// index after the handshake: it said so this session (an index
// notification), or its binary does (IndexProtocol). Not under srvMu: it may
// run a process.
func (h *Harness) speaksIndex(s Server) bool {
	h.srvMu.Lock()
	seen := h.indexSeen[s.ID]
	h.srvMu.Unlock()
	if seen {
		return true
	}
	if !asksIndex(s) {
		return false
	}
	ok, _, err := IndexProtocol(s.Command)
	if err != nil {
		log.Printf("dun: %s: index protocol not known: %v", s.ID, err)
	}
	return ok
}

// excludeArgs are --exclude flags for dirs.
func excludeArgs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		out = append(out, "--exclude", d)
	}
	return out
}

// markIndexBuilding records that id's index is being built, from now. A
// server with no index protocol has no index state.
func (h *Harness) markIndexBuilding(id string, building bool) {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	if !building {
		delete(h.index, id)
		return
	}
	if h.index == nil {
		h.index = map[string]*indexState{}
	}
	h.index[id] = &indexState{state: IndexBuilding, since: time.Now()}
}

// noteIndex takes an index notification from id. It runs on the MCP
// client's read goroutine, so what follows from it (a restart, a config
// write) runs on its own.
func (h *Harness) noteIndex(id string, n indexNote) {
	h.srvMu.Lock()
	if h.index == nil {
		h.index = map[string]*indexState{}
	}
	if h.indexSeen == nil {
		h.indexSeen = map[string]bool{}
	}
	h.indexSeen[id] = true
	prev := h.index[id]
	st := &indexState{state: n.State, note: n, since: time.Now()}
	if n.State == IndexBuilding && prev != nil && prev.state == IndexBuilding {
		st.since = prev.since
	}
	h.index[id] = st
	h.srvMu.Unlock()
	if n.State == IndexBuilding {
		return
	}
	go h.afterIndex(id, n)
}

// afterIndex acts on a finished build and reports it (Config.OnServerIndex).
func (h *Harness) afterIndex(id string, n indexNote) {
	msg := ""
	switch {
	case n.State == IndexReady && n.Large != nil:
		msg = h.largeWarning(id, n)
	case n.State == IndexReady:
		msg = h.nestedWarning(id, n.Index)
	case n.Limit > 0 && n.Index != nil:
		msg = h.retryWithExcludes(id, n)
	default:
		msg = fmt.Sprintf("%s: the index was not built — %s", id, oneLine(n.Error, 300))
		if n.Remedy != "" {
			msg += "\nremedy: " + n.Remedy
		}
	}
	if msg != "" && n.State == IndexFailed {
		// The model's poly-lsp calls answer with the failure, or a
		// restart changes what they see: it hears what dun did, once. A
		// warning on a working index is the person's, not the model's.
		h.Aside("[" + id + " index] " + msg)
	}
	if cb := h.cfg.OnServerIndex; cb != nil {
		cb(id, msg)
	}
}

// retryWithExcludes is a build that stopped at its file limit: leave out the
// obvious directories and start again, once per session; else report.
func (h *Harness) retryWithExcludes(id string, n indexNote) string {
	what := fmt.Sprintf("%s indexable files, limit %s", Grouped(n.Index.Files), Grouped(n.Limit))
	dirs := chooseExcludes(*n.Index, n.Limit)
	h.srvMu.Lock()
	retried := h.indexRetried[id]
	if len(dirs) > 0 && !retried {
		if h.indexRetried == nil {
			h.indexRetried = map[string]bool{}
		}
		h.indexRetried[id] = true
	}
	h.srvMu.Unlock()
	slash := h.slashFor(id)
	switch {
	case retried:
		return fmt.Sprintf("%s: the index failed again, with this project's excludes (%s) — %s.\n%s\nremedy: %s; /%s exclude <dir> leaves another directory out, /%s include <dir> takes one back",
			id, strings.Join(h.excludesOf(id), ", "), what, indexPostMortem(*n.Index), n.Remedy, slash, slash)
	case len(dirs) == 0:
		return fmt.Sprintf("%s: the index stopped at its limit — %s. No directory is an obvious one to leave out, so dun changed nothing; the code tools answer with this error until one is.\n%s\nremedy: %s; in dun, /%s exclude <dir> (saved for this project)",
			id, what, indexPostMortem(*n.Index), n.Remedy, slash)
	}
	var keep []string
	for _, d := range dirs {
		keep = append(keep, d.Dir)
	}
	saved, err := h.addExcludes(id, keep)
	if err != nil {
		return fmt.Sprintf("%s: the index stopped at its limit — %s. dun would leave out %s, but could not save that: %v\n%s",
			id, what, strings.Join(keep, ", "), err, indexPostMortem(*n.Index))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: the index stopped at its limit — %s. dun left out, for this project:", id, what)
	for _, d := range dirs {
		fmt.Fprintf(&b, "\n  %s — %s", d.Dir, excludeReason(d))
	}
	fmt.Fprintf(&b, "\n%s is restarting without %s. Saved in %s.", id, them(len(dirs)), saved)
	undo := make([]string, 0, len(dirs))
	for _, d := range keep {
		undo = append(undo, "/"+slash+" include "+cleanExclude(d))
	}
	fmt.Fprintf(&b, "\nTo undo: %s.", strings.Join(undo, ", "))
	return b.String()
}

// them is "it" or "them".
func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// excludeReason says why a directory was left out: its files, its share of
// the index, and the checkouts in it.
func excludeReason(d IndexDir) string {
	s := fmt.Sprintf("%s files, %d%% of the index", Grouped(d.Files), int(d.Share*100+0.5))
	switch {
	case d.NestedCheckouts == 1:
		s += ", another git checkout"
	case d.NestedCheckouts > 1:
		s += fmt.Sprintf(", %d other git checkouts", d.NestedCheckouts)
	}
	return s
}

// nestedWarning is the quiet line for a ready index that holds another git
// checkout, or "".
func (h *Harness) nestedWarning(id string, rep *IndexReport) string {
	if rep == nil {
		return ""
	}
	var nested []IndexDir
	for _, d := range rep.Heaviest {
		if d.NestedCheckouts > 0 {
			nested = append(nested, d)
		}
	}
	if len(nested) == 0 {
		return ""
	}
	slash := h.slashFor(id)
	var parts, cmds []string
	for _, d := range nested {
		parts = append(parts, d.Dir+" ("+excludeReason(d)+")")
		cmds = append(cmds, "/"+slash+" exclude "+cleanExclude(d.Dir))
	}
	return fmt.Sprintf("%s: the index holds %s. To leave it out: %s", id, strings.Join(parts, "; "), strings.Join(cmds, ", "))
}

// largeWarning is the quiet line for a ready index over the server's
// warning threshold, once per session: the size, where the files are, and
// the command that leaves the obvious directories out. Nothing is excluded
// on a warning: the index is built, and working.
func (h *Harness) largeWarning(id string, n indexNote) string {
	h.srvMu.Lock()
	said := h.indexLarge[id]
	if h.indexLarge == nil {
		h.indexLarge = map[string]bool{}
	}
	h.indexLarge[id] = true
	h.srvMu.Unlock()
	if said {
		return ""
	}
	slash := h.slashFor(id)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: a large workspace — %s indexable files (it warns above %s). The index is built.",
		id, Grouped(n.Large.Files), Grouped(n.Large.Threshold))
	if n.Index != nil && len(n.Index.Heaviest) > 0 {
		b.WriteString("\n" + indexPostMortem(*n.Index))
	}
	var cmds []string
	if n.Index != nil {
		for _, d := range obviousExcludes(*n.Index) {
			cmds = append(cmds, "/"+slash+" exclude "+cleanExclude(d.Dir))
		}
	}
	if len(cmds) > 0 {
		fmt.Fprintf(&b, "\nTo leave out the obvious ones, for this project: %s (/%s include <dir> undoes it).", strings.Join(cmds, ", "), slash)
	} else {
		fmt.Fprintf(&b, "\nTo leave a directory out, for this project: /%s exclude <dir> (/%s include <dir> undoes it).", slash, slash)
	}
	return b.String()
}

// indexPostMortem is the report as rows, for a person: share, directory,
// files, bytes, nested checkouts.
func indexPostMortem(r IndexReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "index: %s files, %s", Grouped(r.Files), HumanBytes(r.Bytes))
	for _, d := range r.Heaviest {
		fmt.Fprintf(&b, "\n  %3d%%  %-24s %8s files  %8s", int(d.Share*100+0.5), d.Dir, Grouped(d.Files), HumanBytes(d.Bytes))
		if d.NestedCheckouts > 0 {
			fmt.Fprintf(&b, "  %d nested checkout(s)", d.NestedCheckouts)
		}
	}
	return b.String()
}

// generatedNames are directory names that hold what a build, a package
// manager or another checkout put there, not the project's own source.
var generatedNames = map[string]bool{
	"worktrees": true, "vendor": true, ".venv": true, "venv": true, "node_modules": true,
	"dist": true, "build": true, "target": true,
}

// looksGenerated reports whether dir ("a/b/") is one of generatedNames, a
// .claude/worktrees, or a .git-like directory (.git-rewrite, .git.bak).
func looksGenerated(dir string) bool {
	d := strings.TrimSuffix(dir, "/")
	base := path.Base(d)
	if generatedNames[base] || strings.Contains("/"+d+"/", "/.claude/worktrees/") {
		return true
	}
	return strings.HasPrefix(base, ".git") && base != ".github" && base != ".gitlab"
}

// dominantShare is the share of the index a generated-looking directory
// must hold to be left out on its name alone.
const dominantShare = 0.5

// chooseExcludes picks the directories to leave out of an index that
// stopped at limit: the obvious ones (a directory holding other git
// checkouts; one over half of the index whose name says it is generated —
// looksGenerated), largest first, as few as bring the count under the limit
// by heaviest's figures. None when the obvious ones together do not: a
// retry that is sure to fail again only adds a second report.
func chooseExcludes(r IndexReport, limit int) []IndexDir {
	cands := obviousExcludes(r)
	left := r.Files
	var out []IndexDir
	for _, d := range cands {
		if left <= limit {
			break
		}
		out = append(out, d)
		left -= d.Files
	}
	if left > limit {
		return nil
	}
	return out
}

// obviousExcludes are heaviest's directories that are plainly not the
// project's own source, largest first: one holding other checkouts, or one
// over half of the index whose name says it is generated.
func obviousExcludes(r IndexReport) []IndexDir {
	var cands []IndexDir
	for _, d := range r.Heaviest {
		if cleanExclude(d.Dir) == "" {
			continue
		}
		if d.NestedCheckouts > 0 || (d.Share > dominantShare && looksGenerated(d.Dir)) {
			cands = append(cands, d)
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].Files > cands[b].Files })
	return cands
}

// cleanExclude is dir as --exclude takes it — relative, inside the root, no
// trailing slash — or "" when it is not one.
func cleanExclude(dir string) string {
	d := filepath.ToSlash(filepath.Clean(strings.TrimSpace(dir)))
	d = strings.TrimSuffix(d, "/")
	if d == "." || d == "" || strings.HasPrefix(d, "/") || d == ".." || strings.HasPrefix(d, "../") {
		return ""
	}
	return d
}

// slashFor is id's slash command ("poly-lsp"), or id.
func (h *Harness) slashFor(id string) string {
	if e, ok := h.extension(id); ok && e.SlashName() != "" {
		return e.SlashName()
	}
	return id
}

// excludesOf is id's excluded directories as configured.
func (h *Harness) excludesOf(id string) []string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	s, _ := h.spec(id)
	return slices.Clone(s.Exclude)
}

// IndexExcludes is the directories left out of id's index, for this project.
func (h *Harness) IndexExcludes(id string) []string { return h.excludesOf(id) }

// addExcludes saves dirs into id's project excludes and restarts it, in the
// background. It returns the file written.
func (h *Harness) addExcludes(id string, dirs []string) (string, error) {
	return h.editExcludes(context.Background(), id, false, func(cur []string) []string {
		for _, d := range dirs {
			if c := cleanExclude(d); c != "" && !slices.Contains(cur, c) {
				cur = append(cur, c)
			}
		}
		return cur
	})
}

// SetIndexExclude leaves dir out of id's index (on) or takes it back (off),
// for this project, and restarts the server when it runs. It reports the
// file written and whether anything changed.
func (h *Harness) SetIndexExclude(ctx context.Context, id, dir string, on bool) (string, bool, error) {
	d := cleanExclude(dir)
	if d == "" {
		return "", false, fmt.Errorf("%q: want a directory inside the workspace, relative to it", dir)
	}
	if _, err := h.knownServer(id); err != nil {
		return "", false, err
	}
	cur := h.excludesOf(id)
	if slices.Contains(cur, d) == on {
		return "", false, nil
	}
	p, err := h.editExcludes(ctx, id, true, func(cur []string) []string {
		if on {
			return append(cur, d)
		}
		return slices.DeleteFunc(cur, func(c string) bool { return c == d })
	})
	return p, err == nil, err
}

// editExcludes rewrites id's project excludes and applies them: a running
// server restarts with them (waited for when wait).
func (h *Harness) editExcludes(ctx context.Context, id string, wait bool, edit func([]string) []string) (string, error) {
	next := edit(h.excludesOf(id))
	p, err := updateLocalServer(h.configDir(), id, func(s *ServerSpec) {
		s.Exclude = next
		if len(next) == 0 {
			s.Exclude = nil // the entry goes when nothing else is in it
		}
	})
	if err != nil {
		return "", err
	}
	apply := func(wait bool) error {
		restart := map[string]bool{}
		if h.mgr.ServerStarted(id) {
			restart[id] = true
		}
		return h.reloadServers(ctx, wait, restart)
	}
	if wait {
		return p, apply(true)
	}
	go func() {
		// The report can land before the start that spawned the server
		// has finished (its tools listed): a restart under it would be
		// skipped as "already starting". Let that start end first.
		for deadline := time.Now().Add(time.Minute); h.isStarting(id) && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
		if err := apply(false); err != nil {
			log.Printf("dun: %s: not restarted with its excludes: %v", id, err)
		}
	}()
	return p, nil
}

// indexOf fills st's index fields from id's index state. Caller holds srvMu.
func (h *Harness) indexOf(id string, st *ServerState) {
	is := h.index[id]
	if is == nil {
		return
	}
	st.Index = is.state
	switch is.state {
	case IndexBuilding:
		st.IndexSeconds = time.Since(is.since).Seconds()
	case IndexReady, IndexFailed:
		st.IndexSeconds = float64(is.note.Ms) / 1000
		if is.state == IndexFailed {
			st.IndexErr = is.note.Error
		}
		if is.note.Index != nil {
			st.IndexFiles = is.note.Index.Files
		}
	}
}
