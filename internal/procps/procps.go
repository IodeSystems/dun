// Package procps is `dun ps`: every process dun is responsible for on this
// machine, attributed to the session it works for (USER 2026-11-17: the
// laptop's CPU and battery go somewhere, poly-lsp-mcp is suspected, and
// plain ps cannot say which session is responsible).
//
// dund names its running sessions (proto.Procs): each engine's pid, the
// trees the session and its sub-agents work in, its jobs' process groups.
// This package reads the process table twice, a second apart, and
//
//   - walks each engine's process tree: the engine, its shell (mcpshell),
//     its poly-lsp proxy, its other tool servers, its background jobs, the
//     commands they run;
//   - finds the shared poly-lsp-mcp daemon (one per machine, an LSP fleet
//     per root: docs/mcp-and-tools.md) and gives each language server to the
//     session whose workspace, worktree or sub-agent tree is its root. A
//     root no live session owns is SESSION (none): the leak to look for;
//   - flags processes that look like dun's and were left to pid 1.
//
// The table is read from /proc on Linux. On macOS, ps gives the table,
// kern.procargs2 the argv, and proc_info (the syscall under libproc, made
// without cgo) the disk IO and the working directory.
package procps

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
)

// Proc is one process as read from the table.
type Proc struct {
	PID, PPID, PGID int
	Argv            []string
	Start           time.Time // zero unknown
	CPU             float64   // seconds of CPU, user and system, so far
	RSS             int64     // resident bytes
	Read, Write     int64     // bytes read from and written to storage so far; -1 unknown
}

// Snapshot is the process table at one moment.
type Snapshot struct {
	At    time.Time
	Procs map[int]*Proc
}

// Rate is a process's use between two snapshots.
type Rate struct {
	CPU              float64 // percent of one core
	ReadBps, WritBps float64 // -1 unknown
}

// Rates is each process's use from prev to cur. A process only in cur, or
// whose start differs (the pid was reused), has none.
func Rates(prev, cur Snapshot) map[int]Rate {
	out := map[int]Rate{}
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 {
		return out
	}
	for pid, c := range cur.Procs {
		p, ok := prev.Procs[pid]
		if !ok || !p.Start.Equal(c.Start) {
			continue
		}
		r := Rate{CPU: max(0, 100*(c.CPU-p.CPU)/dt), ReadBps: -1, WritBps: -1}
		if c.Read >= 0 && p.Read >= 0 && c.Write >= 0 && p.Write >= 0 {
			r.ReadBps = max(0, float64(c.Read-p.Read)/dt)
			r.WritBps = max(0, float64(c.Write-p.Write)/dt)
		}
		out[pid] = r
	}
	return out
}

// Groups a row belongs to besides a session.
const (
	GroupNone    = "(none)"    // no live session: an orphan, or an LSP root nobody owns
	GroupShared  = "(shared)"  // the per-machine daemons themselves
	GroupService = "(service)" // dund and what it runs outside any session
)

// Row is one process, attributed.
type Row struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	Group string `json:"group"` // a session id, or one of the Group constants
	// Session is the group as shown: short id and workspace name.
	Session string `json:"session"`
	Job     string `json:"job"`
	// Root is a language server's root (its working directory).
	Root   string `json:"root,omitempty"`
	Orphan bool   `json:"orphan,omitempty"`
	Argv   string `json:"argv"`
	// Roots are the roots a poly-lsp-mcp daemon holds, by owning session;
	// RootsErr says why they are missing.
	Roots    []Root `json:"roots,omitempty"`
	RootsErr string `json:"roots_error,omitempty"`
	// Rag is retrieval in this process (dund's, rag.go): its embed backend,
	// its active indexes, and the ones idle RagIdleAfter, which the table
	// folds into one line.
	RagEmbed *proto.ProcRagEmbed `json:"rag_embed,omitempty"`
	Rag      []RagIndex          `json:"rag,omitempty"`
	RagIdle  []RagIndex          `json:"rag_idle,omitempty"`

	CPU     float64 `json:"cpu"` // percent of one core; -1 before a rate
	RSS     int64   `json:"rss"`
	ReadBps float64 `json:"read_bps"` // -1 unknown
	WritBps float64 `json:"write_bps"`
	AgeSec  int64   `json:"age_sec"` // -1 unknown
}

// Attribute turns the table into rows: every process dun is responsible
// for, given dund's sessions (list may be nil: dund is not running). cwd
// reads a process's working directory ("" unknown).
func Attribute(snap Snapshot, list *proto.ProcsList, cwd func(int) string) []Row {
	a := attributor{snap: snap, cwd: cwd, kids: map[int][]int{}, done: map[int]bool{}}
	for pid, p := range snap.Procs {
		a.kids[p.PPID] = append(a.kids[p.PPID], pid)
	}
	for _, k := range a.kids {
		sort.Ints(k)
	}
	var sessions []proto.ProcSession
	dundPID := 0
	if list != nil {
		sessions, dundPID = list.Sessions, list.DundPID
	}
	a.sessions = sessions
	for _, s := range sessions {
		if s.EnginePID == 0 || snap.Procs[s.EnginePID] == nil {
			continue
		}
		a.walkSession(s)
	}
	if p := snap.Procs[dundPID]; p != nil && dundPID != 0 {
		a.add(p, GroupService, GroupService, "dund", "", false)
		for _, c := range a.kids[dundPID] {
			if !a.done[c] {
				a.walkPlain(c, GroupService, GroupService, "", false)
			}
		}
	}
	// The shared daemons: wherever they are (detached, so under pid 1).
	pids := sortedPIDs(snap)
	for _, pid := range pids {
		p := snap.Procs[pid]
		if a.done[pid] {
			continue
		}
		switch Role(p.Argv) {
		case rolePolyDaemon:
			a.add(p, GroupShared, GroupShared, "poly-lsp daemon", "", false)
			for _, c := range a.kids[pid] {
				a.walkLSP(c, false)
			}
		case roleRaglitDaemon:
			a.add(p, GroupShared, GroupShared, "raglit daemon", "", false)
			for _, c := range a.kids[pid] {
				a.walkPlain(c, GroupShared, GroupShared, "raglit daemon › ", false)
			}
		}
	}
	// Orphans: dun's kind of process, left to pid 1 (or a subreaper).
	for _, pid := range pids {
		p := snap.Procs[pid]
		if a.done[pid] || !a.leftToInit(p) {
			continue
		}
		role := Role(p.Argv)
		switch {
		case isLSP(p.Argv):
			a.walkLSP(pid, true)
		case dunsRoles[role]:
			a.walkPlain(pid, GroupNone, GroupNone, "", true)
		}
	}
	return a.rows
}

type attributor struct {
	snap     Snapshot
	cwd      func(int) string
	kids     map[int][]int
	done     map[int]bool
	sessions []proto.ProcSession
	rows     []Row
}

func (a *attributor) add(p *Proc, group, label, job, root string, orphan bool) {
	a.done[p.PID] = true
	r := Row{PID: p.PID, PPID: p.PPID, Group: group, Session: label, Job: job, Root: root, Orphan: orphan,
		Argv: strings.Join(p.Argv, " "), CPU: -1, RSS: p.RSS, ReadBps: -1, WritBps: -1, AgeSec: -1}
	if !p.Start.IsZero() {
		r.AgeSec = int64(a.snap.At.Sub(p.Start).Seconds())
	}
	a.rows = append(a.rows, r)
}

// walkSession labels the engine's tree.
func (a *attributor) walkSession(s proto.ProcSession) {
	label := SessionLabel(s.ID, s.Workspace)
	jobs := map[int]proto.ProcJob{}
	for _, j := range s.Jobs {
		if j.PGID != 0 {
			jobs[j.PGID] = j
		}
	}
	var walk func(pid int, prefix string)
	walk = func(pid int, prefix string) {
		p := a.snap.Procs[pid]
		if p == nil || a.done[pid] {
			return
		}
		if j, ok := jobs[p.PGID]; ok && !strings.HasPrefix(prefix, "job #") {
			a.add(p, s.ID, label, "background job #"+itoa(j.ID)+" `"+clip(oneLine(j.Command), 40)+"`", "", false)
			a.descend(pid, func(c int) { walk(c, "job #"+itoa(j.ID)+" › ") })
			return
		}
		if prefix != "" {
			a.add(p, s.ID, label, prefix+Base(p.Argv), "", false)
			a.descend(pid, func(c int) { walk(c, prefix) })
			return
		}
		role := Role(p.Argv)
		agent := agentOf(p.Argv, s)
		switch {
		case pid == s.EnginePID:
			a.add(p, s.ID, label, "engine", "", false)
			a.descend(pid, func(c int) { walk(c, "") })
		case role == roleShell:
			a.add(p, s.ID, label, agent+"shell (mcpshell)", "", false)
			a.descend(pid, func(c int) { walk(c, "shell › ") })
		case role == rolePolyProxy:
			a.add(p, s.ID, label, agent+"poly-lsp proxy", "", false)
			a.descend(pid, func(c int) { walk(c, "poly-lsp › ") })
		case role == rolePolyOwn:
			// No --daemon: this proxy runs its own fleet, its children are LSPs.
			a.add(p, s.ID, label, agent+"poly-lsp (own LSPs)", "", false)
			for _, c := range a.kids[pid] {
				a.walkLSPIn(c, s.ID, label, false)
			}
		case role == roleEngine:
			a.add(p, s.ID, label, agent+"child engine", "", false)
			a.descend(pid, func(c int) { walk(c, "child engine › ") })
		case role == roleRaglitClient:
			a.add(p, s.ID, label, agent+"raglit client", "", false)
			a.descend(pid, func(c int) { walk(c, "raglit › ") })
		default:
			a.add(p, s.ID, label, agent+"tool server "+Base(p.Argv), "", false)
			a.descend(pid, func(c int) { walk(c, Base(p.Argv)+" › ") })
		}
	}
	walk(s.EnginePID, "")
}

func (a *attributor) descend(pid int, f func(int)) {
	for _, c := range a.kids[pid] {
		f(c)
	}
}

// walkPlain labels a tree by role, every row in one group.
func (a *attributor) walkPlain(pid int, group, label, prefix string, orphan bool) {
	p := a.snap.Procs[pid]
	if p == nil || a.done[pid] {
		return
	}
	job := prefix + Base(p.Argv)
	if prefix == "" {
		job = roleName(Role(p.Argv), p.Argv)
		if orphan {
			job = "orphaned " + job
		}
	}
	a.add(p, group, label, job, "", orphan)
	if Role(p.Argv) == rolePolyOwn || Role(p.Argv) == rolePolyDaemon {
		for _, c := range a.kids[pid] {
			a.walkLSPIn(c, group, label, orphan)
		}
		return
	}
	next := prefix
	if next == "" {
		next = Base(p.Argv) + " › "
	}
	for _, c := range a.kids[pid] {
		a.walkPlain(c, group, label, next, orphan)
	}
}

// walkLSP labels a language server (and its children) by the session that
// owns its root.
func (a *attributor) walkLSP(pid int, orphan bool) {
	p := a.snap.Procs[pid]
	if p == nil || a.done[pid] {
		return
	}
	root := a.cwd(pid)
	group, label := GroupNone, GroupNone
	if !orphan {
		if s, ok := Owner(root, a.sessions); ok {
			group, label = s.ID, SessionLabel(s.ID, s.Workspace)
		}
	}
	a.walkLSPIn(pid, group, label, orphan)
}

func (a *attributor) walkLSPIn(pid int, group, label string, orphan bool) {
	p := a.snap.Procs[pid]
	if p == nil || a.done[pid] {
		return
	}
	root := a.cwd(pid)
	job := "lsp " + Base(p.Argv) + " " + Tilde(root)
	if orphan {
		job = "orphaned " + job
	}
	a.add(p, group, label, job, root, orphan)
	var sub func(int)
	sub = func(c int) {
		q := a.snap.Procs[c]
		if q == nil || a.done[c] {
			return
		}
		a.add(q, group, label, "lsp "+Base(p.Argv)+" › "+Base(q.Argv), root, orphan)
		a.descend(c, sub)
	}
	a.descend(pid, sub)
}

// leftToInit: the process's parent is pid 1 or a subreaper (systemd --user
// on Linux, launchd on macOS).
func (a *attributor) leftToInit(p *Proc) bool {
	if p.PPID == 1 {
		return true
	}
	pp := a.snap.Procs[p.PPID]
	if pp == nil {
		return p.PPID <= 1
	}
	b := Base(pp.Argv)
	return b == "systemd" || b == "launchd"
}

// Owner is the session a root belongs to: the one whose workspace, worktree
// or sub-agent tree is root, else holds root, the deepest such first.
func Owner(root string, sessions []proto.ProcSession) (proto.ProcSession, bool) {
	if root == "" {
		return proto.ProcSession{}, false
	}
	root = resolve(root)
	best, bestLen := -1, -1
	for i, s := range sessions {
		trees := []string{s.Workspace, s.Worktree}
		for _, ag := range s.Agents {
			trees = append(trees, ag.Dir)
		}
		for _, t := range trees {
			if t == "" {
				continue
			}
			t = resolve(t)
			if (root == t || strings.HasPrefix(root, t+string(filepath.Separator))) && len(t) > bestLen {
				best, bestLen = i, len(t)
			}
		}
	}
	if best < 0 {
		return proto.ProcSession{}, false
	}
	return sessions[best], true
}

// agentOf is "sub-agent #N " when the argv names a sub-agent's tree.
func agentOf(argv []string, s proto.ProcSession) string {
	for _, ag := range s.Agents {
		d := filepath.Clean(ag.Dir)
		for _, arg := range argv {
			if _, v, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(arg, "-") {
				arg = v
			}
			if arg != "" && filepath.Clean(arg) == d {
				return "sub-agent #" + itoa(ag.ID) + " "
			}
		}
	}
	return ""
}

// Roles from argv.
const (
	roleOther        = ""
	roleEngine       = "engine"
	roleDund         = "dund"
	roleShell        = "shell"
	rolePolyDaemon   = "poly-daemon"
	rolePolyProxy    = "poly-proxy"
	rolePolyOwn      = "poly-own"
	roleRaglitDaemon = "raglit-daemon"
	roleRaglitClient = "raglit-client"
)

// dunsRoles are the processes dun starts that should not outlive their
// parent.
var dunsRoles = map[string]bool{roleEngine: true, roleShell: true, rolePolyProxy: true,
	rolePolyOwn: true, roleRaglitClient: true}

// Role says what a process is from its argv.
func Role(argv []string) string {
	if len(argv) == 0 {
		return roleOther
	}
	b := filepath.Base(argv[0])
	has := func(s string) bool {
		for _, a := range argv[1:] {
			if a == s {
				return true
			}
		}
		return false
	}
	first := ""
	if len(argv) > 1 {
		first = argv[1]
	}
	switch {
	case b == "dun" && has("--engine"):
		return roleEngine
	case b == "dun" && first == "dund":
		return roleDund
	case b == "mcpshell":
		return roleShell
	case strings.HasPrefix(b, "poly-lsp-mcp"):
		switch {
		case first == "daemon":
			return rolePolyDaemon
		case first == "mcp" && has("--daemon"):
			return rolePolyProxy
		case first == "mcp":
			return rolePolyOwn
		}
	case b == "raglit" && first == "daemon":
		return roleRaglitDaemon
	case b == "raglit" && first == "serve":
		return roleRaglitClient
	}
	return roleOther
}

func roleName(role string, argv []string) string {
	switch role {
	case roleEngine:
		return "engine"
	case roleDund:
		return "dund"
	case roleShell:
		return "shell (mcpshell)"
	case rolePolyDaemon:
		return "poly-lsp daemon"
	case rolePolyProxy:
		return "poly-lsp proxy"
	case rolePolyOwn:
		return "poly-lsp (own LSPs)"
	case roleRaglitDaemon:
		return "raglit daemon"
	case roleRaglitClient:
		return "raglit client"
	}
	return Base(argv)
}

// knownLSPs are language servers poly-lsp-mcp starts, by Base.
var knownLSPs = map[string]bool{"gopls": true, "typescript-language-server": true, "tsserver": true,
	"tsgo": true, "pylsp": true, "pyright-langserver": true, "basedpyright-langserver": true,
	"rust-analyzer": true, "clangd": true, "jdtls": true, "kotlin-language-server": true,
	"lua-language-server": true, "bash-language-server": true, "zls": true, "solargraph": true,
	"ruby-lsp": true, "elixir-ls": true, "metals": true, "sourcekit-lsp": true,
	"vscode-json-language-server": true, "yaml-language-server": true}

func isLSP(argv []string) bool { return knownLSPs[Base(argv)] }

// interpreters run the program named by their first non-flag argument.
var interpreters = map[string]bool{"node": true, "python": true, "python3": true, "ruby": true,
	"java": true, "bun": true, "deno": true, "perl": true, "sh": true, "bash": true}

// Base is the program a process runs: argv[0]'s base name, or for an
// interpreter its script's.
func Base(argv []string) string {
	if len(argv) == 0 {
		return "?"
	}
	b := filepath.Base(argv[0])
	if interpreters[b] {
		for _, a := range argv[1:] {
			if a == "-c" || a == "-m" || a == "-jar" {
				break
			}
			if !strings.HasPrefix(a, "-") {
				return filepath.Base(a)
			}
		}
	}
	return b
}

// SessionLabel is a session as a row shows it: its short id and its
// workspace's name.
func SessionLabel(id, workspace string) string {
	return ShortID(id) + " " + filepath.Base(workspace)
}

// ShortID shortens a session id: "20261021-222836" (a start time, to the
// second) to "1021-2228"; another id to its first 8 characters.
func ShortID(id string) string {
	if len(id) == 15 && id[8] == '-' && allDigits(id[:8]) && allDigits(id[9:]) {
		return id[4:8] + "-" + id[9:13]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// home is replaced by ~ in roots; a variable so tests fix it.
var home = userHome()

// Tilde writes a path under the home directory with ~.
func Tilde(p string) string {
	switch {
	case p == "":
		return "?"
	case home != "" && p == home:
		return "~"
	case home != "" && strings.HasPrefix(p, home+string(filepath.Separator)):
		return "~" + p[len(home):]
	}
	return p
}

func sortedPIDs(s Snapshot) []int {
	out := make([]int, 0, len(s.Procs))
	for pid := range s.Procs {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// resolve is p with symlinks resolved (macOS's /tmp is /private/tmp), else
// cleaned.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func itoa(n int) string { return strconv.Itoa(n) }

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// Apply sets each row's CPU and IO from rates.
func Apply(rows []Row, rates map[int]Rate) {
	for i := range rows {
		if r, ok := rates[rows[i].PID]; ok {
			rows[i].CPU, rows[i].ReadBps, rows[i].WritBps = r.CPU, r.ReadBps, r.WritBps
		}
	}
}
