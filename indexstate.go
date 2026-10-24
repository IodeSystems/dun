package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
//	 nestedCheckouts}], hint}, "error", "remedy", "budget", "read"}
//
// "budget" is on a build that ran out of its startup budget,
// --index-budget (node_query's terms: "30000ms" wall-clock, the default, or
// "Nops", one per file read); "read" is the files it read within it. Past
// the budget the walk goes on counting, so heaviest covers every file: where
// they are. The remedy names a budget that would have fitted
// ("--index-budget 48000ms (now 30000ms)"). A short-lived build before it
// failed at a file cap instead (--index-max-files, "limit"); dun reads that
// too.
//
// What dun does with it:
//
//   - building: the server is up but its index is not; Servers() says
//     Index "building" and for how long ("poly-lsp indexing 12s").
//   - ready: Index "ready". A directory in heaviest that holds other git
//     checkouts gets one quiet line, with the command that leaves it out.
//   - failed, out of budget: dun picks the obvious directories to leave out
//     (chooseExcludes: as few as bring the files under what was read in
//     budget), saves them in the project's server config
//     (ServerSpec.Exclude, $DUN_HOME/projects/<id>/dun.local.json), and
//     restarts the server with --exclude for each. With nothing obvious it
//     raises the budget instead — the remedy's, else the spent one scaled by
//     files counted over files read, plus a fifth — saved as
//     ServerSpec.IndexBudget. Either way it says what it did, why, and how
//     to undo it (/poly-lsp include <dir>, /poly-lsp budget default). Once
//     per session; a second failure is reported, not retried.
//   - failed at the file cap (the intermediate build): the same excludes;
//     with nothing obvious it changes nothing and reports.
//
// The initialize result also says "index": {"state": "building"}, but
// agentkit's mcpmgr (v0.10.1) keeps only the capabilities of that result.
// So "building" comes from the binary instead (IndexProtocol): a
// poly-lsp-mcp whose `mcp --help` lists --index-budget or --index-max-files
// speaks this protocol and takes --exclude; only the first takes
// --index-budget. An older one builds before it answers (Starting covers
// it), sends no notification, and rejects --exclude; dun passes it neither
// and shows no index state — it behaves as before. Any server that sends a
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
	// LeftOut is what the build did not look into, and why: git decides the
	// file set in a repo (poly-lsp 5ad7920), so ignored directories and
	// nested repositories are named here rather than counted.
	LeftOut []IndexLeftOut `json:"leftOut,omitempty"`
	// LeftOutBy is every reason in LeftOut with its true count (LeftOut
	// holds at most 20 entries per reason) and the way to refine or revert
	// it. Absent from an older poly-lsp.
	LeftOutBy []IndexLeftOutBy `json:"leftOutBy,omitempty"`
}

// IndexLeftOutBy is one left-out reason's total and its hint.
type IndexLeftOutBy struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Hint   string `json:"hint,omitempty"`
}

// IndexLeftOut is one directory or file the index left out. Reason is one of
// "git-ignored", "worktree of this repo", "another repo", "excluded",
// "unreadable", "skipped dir", "over size", "minified", "LFS pointer (not
// downloaded)", or a "symlink ..." one.
type IndexLeftOut struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
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
	Budget string       `json:"budget,omitempty"` // the budget spent: "30000ms"
	Read   int          `json:"read,omitempty"`   // files read within it
	Limit  int          `json:"limit,omitempty"`  // the intermediate build's file cap
	// A root that is not a project (the home directory, /, a directory of
	// clones) fails at once with Reason "not a project": Repos are the git
	// repositories found in it, relative, at most 20; ReposTotal how many
	// there are. The remedy is --root <repo> or --index-anyway.
	Reason     string   `json:"reason,omitempty"`
	Repos      []string `json:"repos,omitempty"`
	ReposTotal int      `json:"reposTotal,omitempty"`
}

// reasonNotProject is the Reason of a root poly-lsp refused as no project.
const reasonNotProject = "not a project"

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

// IndexSupport is what a poly-lsp-mcp binary takes of the index protocol.
type IndexSupport struct {
	Protocol bool // a background index, its report, and --exclude
	Budget   bool // --index-budget (the intermediate build had a file cap)
	Anyway   bool // --index-anyway (index a root that is not a project)
	Daemon   bool // --daemon that keeps the file-access hook (daemonMarker)
}

// daemonMarker is in the --hook help of a poly-lsp-mcp whose --daemon proxy
// runs the hook itself (poly-lsp-mcp e9b0020). An earlier --daemon dropped
// the hook, and with it dun's AGENTS.md guard, so dun passes --daemon only to
// a build that says this. A substring: with $POLY_LSP_HOOK set, cobra
// appends the default to the line.
const daemonMarker = "--daemon: the proxy runs it"


// indexProbes caches IndexProtocol by the binary's path, size and mtime: a
// rebuilt binary is asked again, an unchanged one is not.
var indexProbes sync.Map // key → IndexSupport

// indexProbeTimeout bounds the `mcp --help` IndexProtocol runs.
const indexProbeTimeout = 5 * time.Second

// IndexProtocol reports what the poly-lsp-mcp named by command (a path, or a
// name looked up on PATH) takes of the index protocol: its initialize
// answers before the index is built, the build's end is a notification, and
// it takes --exclude; with Budget, --index-budget too. It runs `<command>
// mcp --help` — a read: LSP_REBUILD_DONE stops a dev build's self-update —
// and looks for --index-budget, or the intermediate build's
// --index-max-files. resolved is the binary that was asked.
func IndexProtocol(command string) (sup IndexSupport, resolved string, err error) {
	resolved, err = exec.LookPath(command)
	if err != nil {
		return sup, "", err
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return sup, resolved, err
	}
	key := fmt.Sprintf("%s|%d|%d", resolved, st.Size(), st.ModTime().UnixNano())
	if v, hit := indexProbes.Load(key); hit {
		return v.(IndexSupport), resolved, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), indexProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "mcp", "--help")
	cmd.Env = append(os.Environ(), "LSP_REBUILD_DONE=1")
	out, runErr := cmd.CombinedOutput()
	if len(out) == 0 && runErr != nil {
		return sup, resolved, fmt.Errorf("%s mcp --help: %w", resolved, runErr)
	}
	help := string(out)
	sup.Budget = strings.Contains(help, "--index-budget")
	sup.Anyway = strings.Contains(help, "--index-anyway")
	sup.Daemon = strings.Contains(help, daemonMarker)
	sup.Protocol = sup.Budget || strings.Contains(help, "--index-max-files")
	indexProbes.Store(key, sup)
	return sup, resolved, nil
}

// speaksIndex reports whether s, about to be spawned from command, builds
// its index after the handshake — it said so this session (an index
// notification), or its binary does (IndexProtocol) — and what else of the
// protocol its binary takes. Not under srvMu: it may run a process.
func (h *Harness) speaksIndex(s Server, command string) (indexes bool, sup IndexSupport) {
	h.srvMu.Lock()
	seen := h.indexSeen[s.ID]
	h.srvMu.Unlock()
	if !asksIndex(s) {
		return seen, sup
	}
	sup, _, err := IndexProtocol(command)
	if err != nil {
		log.Printf("dun: %s: index protocol not known: %v", s.ID, err)
	}
	return sup.Protocol || seen, sup
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
	case n.State == IndexReady:
		msg = h.nestedWarning(id, n.Index)
	case n.Reason == reasonNotProject:
		msg = h.askNotProject(id, n)
	case (n.Budget != "" || n.Limit > 0) && n.Index != nil:
		msg = h.retryIndex(id, n)
	default:
		msg = fmt.Sprintf("%s: the index was not built — %s", id, oneLine(n.Error, 300))
		if n.Remedy != "" {
			msg += "\nremedy: " + n.Remedy
		}
	}
	if msg != "" && n.State == IndexFailed {
		// The model's poly-lsp calls answer with the failure, or a
		// restart changes what they see: it hears what dun did, once. A
		// notice on a working index is the person's, not the model's.
		h.Aside("[" + id + " index] " + msg)
	}
	if cb := h.cfg.OnServerIndex; cb != nil {
		cb(id, msg)
	}
}

// retryIndex is a build that ran out of budget (or, from the intermediate
// build, hit its file cap): leave out the obvious directories, else raise
// the budget, and start again — once per session; else report.
func (h *Harness) retryIndex(id string, n indexNote) string {
	rep := *n.Index
	// The files that fit: what was read within the budget, or the cap.
	what, target := "", n.Limit
	if n.Budget != "" {
		what = fmt.Sprintf("the index did not fit its startup budget — %s read %s of %s indexable files",
			n.Budget, Grouped(n.Read), Grouped(rep.Files))
		target = n.Read
	} else {
		what = fmt.Sprintf("the index stopped at its limit — %s indexable files, limit %s", Grouped(rep.Files), Grouped(n.Limit))
	}
	dirs := chooseExcludes(rep, target)
	raise := ""
	if len(dirs) == 0 && n.Budget != "" {
		raise = raisedBudget(n)
	}
	h.srvMu.Lock()
	retried := h.indexRetried[id]
	if (len(dirs) > 0 || raise != "") && !retried {
		if h.indexRetried == nil {
			h.indexRetried = map[string]bool{}
		}
		h.indexRetried[id] = true
	}
	h.srvMu.Unlock()
	slash := h.slashFor(id)
	switch {
	case retried:
		return fmt.Sprintf("%s: %s, again, after dun's change (%s).\n%s\nremedy: %s\nin dun, for this project: /%s exclude <dir>, /%s include <dir>, /%s budget <Nms|Nops|default>",
			id, what, h.indexConfigNow(id), indexPostMortem(rep), n.Remedy, slash, slash, slash)
	case len(dirs) == 0 && raise == "":
		return fmt.Sprintf("%s: %s. No directory is an obvious one to leave out, so dun changed nothing; the code tools answer with this error until one is.\n%s\nremedy: %s; in dun, /%s exclude <dir> (saved for this project)",
			id, what, indexPostMortem(rep), n.Remedy, slash)
	case raise != "":
		saved, err := h.editIndexSpec(context.Background(), id, false, func(s *ServerSpec) { s.IndexBudget = raise })
		if err != nil {
			return fmt.Sprintf("%s: %s. dun would raise the budget to %s, but could not save that: %v\n%s", id, what, raise, err, indexPostMortem(rep))
		}
		return fmt.Sprintf("%s: %s, and no directory is an obvious one to leave out. dun raised the budget to %s, for this project; %s is restarting with it. Saved in %s.\n%s\nTo undo: /%s budget default. To leave a directory out instead: /%s exclude <dir>.",
			id, what, raise, id, saved, indexPostMortem(rep), slash, slash)
	}
	var keep []string
	for _, d := range dirs {
		keep = append(keep, d.Dir)
	}
	saved, err := h.addExcludes(id, keep)
	if err != nil {
		return fmt.Sprintf("%s: %s. dun would leave out %s, but could not save that: %v\n%s",
			id, what, strings.Join(keep, ", "), err, indexPostMortem(rep))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s. dun left out, for this project:", id, what)
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

// indexConfigNow is id's index settings for this project, as a phrase.
func (h *Harness) indexConfigNow(id string) string {
	var parts []string
	if ex := h.excludesOf(id); len(ex) > 0 {
		parts = append(parts, "excluded: "+strings.Join(ex, ", "))
	}
	if b := h.IndexBudget(id); b != "" {
		parts = append(parts, "budget "+b)
	}
	if len(parts) == 0 {
		return "nothing set"
	}
	return strings.Join(parts, "; ")
}

// remedyBudget is the budget poly-lsp's remedy names: "--index-budget
// 48000ms (now 30000ms)".
var remedyBudget = regexp.MustCompile(`--index-budget (\d+(?:ms|ops)?) \(now`)

// budgetPattern is a budget as --index-budget takes it.
var budgetPattern = regexp.MustCompile(`^(\d+)(ms|ops)?$`)

// raisedBudget is a budget that would have fitted the build in n: the one
// its remedy names, else the spent one scaled by files counted over files
// read, plus a fifth (poly-lsp's own rule), in whole seconds.
func raisedBudget(n indexNote) string {
	if m := remedyBudget.FindStringSubmatch(n.Remedy); m != nil {
		return m[1]
	}
	m := budgetPattern.FindStringSubmatch(n.Budget)
	if m == nil || n.Index == nil {
		return ""
	}
	v, _ := strconv.Atoi(m[1])
	scale := float64(n.Index.Files) / float64(max(n.Read, 1)) * 1.2
	if m[2] == "ops" {
		return fmt.Sprintf("%dops", int(float64(v)*scale)+1)
	}
	return fmt.Sprintf("%dms", int(math.Ceil(float64(v)*scale/1000))*1000)
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

// chooseExcludes picks the directories to leave out of an index that did
// not fit — limit is the files that do: those read within the budget, or
// the cap. The obvious ones (a directory holding other git checkouts; one
// over half of the index whose name says it is generated — looksGenerated),
// largest first, as few as bring the count to limit or under by heaviest's
// figures. None when the obvious ones together do not: a retry that is sure
// to fail again only adds a second report.
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

// IndexBudget is id's index startup budget for this project, "" for the
// server's default.
func (h *Harness) IndexBudget(id string) string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	s, _ := h.spec(id)
	return s.IndexBudget
}

// SetIndexBudget sets id's index startup budget for this project, as
// --index-budget takes it ("48000ms", "20000ops", a bare number of ms, "0"
// unbounded); "" or "default" goes back to the server's default. A running
// server restarts with it. It reports the file written and whether anything
// changed.
func (h *Harness) SetIndexBudget(ctx context.Context, id, budget string) (string, bool, error) {
	b := strings.ToLower(strings.TrimSpace(budget))
	if b == "default" {
		b = ""
	}
	if b != "" && !budgetPattern.MatchString(b) {
		return "", false, fmt.Errorf("%q: want Nms (wall-clock; a bare number is ms), Nops (files read), 0 (unbounded) or default", budget)
	}
	if _, err := h.knownServer(id); err != nil {
		return "", false, err
	}
	if h.IndexBudget(id) == b {
		return "", false, nil
	}
	p, err := h.editIndexSpec(ctx, id, true, func(s *ServerSpec) { s.IndexBudget = b })
	return p, err == nil, err
}

// editExcludes rewrites id's project excludes and applies them (editIndexSpec).
func (h *Harness) editExcludes(ctx context.Context, id string, wait bool, edit func([]string) []string) (string, error) {
	next := edit(h.excludesOf(id))
	return h.editIndexSpec(ctx, id, wait, func(s *ServerSpec) {
		s.Exclude = next
		if len(next) == 0 {
			s.Exclude = nil // the entry goes when nothing else is in it
		}
	})
}

// editIndexSpec edits id's project entry and applies it: a running server
// restarts (waited for when wait).
func (h *Harness) editIndexSpec(ctx context.Context, id string, wait bool, edit func(*ServerSpec)) (string, error) {
	p, err := updateLocalServer(h.configDir(), id, edit)
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
			log.Printf("dun: %s: not restarted with its index settings: %v", id, err)
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
			st.IndexLeftOut = is.note.Index.LeftOut
			st.IndexLeftOutBy = is.note.Index.LeftOutBy
		}
	}
}
