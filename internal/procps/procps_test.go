package procps

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func fake(procs ...*Proc) Snapshot {
	s := Snapshot{At: t0, Procs: map[int]*Proc{}}
	for _, p := range procs {
		if p.Start.IsZero() {
			p.Start = t0.Add(-time.Hour)
		}
		if p.PGID == 0 {
			p.PGID = p.PID
		}
		s.Procs[p.PID] = p
	}
	return s
}

func pr(pid, ppid int, argv ...string) *Proc {
	return &Proc{PID: pid, PPID: ppid, Argv: argv, Read: -1, Write: -1}
}

// A fake machine: dund with two engines (one of another user's session),
// session A's shell, proxies, a tool server, a job; the shared daemon with
// language servers for A's workspace, A's sub-agent tree and a root no
// session works in; and leftovers under pid 1.
func machine() (Snapshot, *proto.ProcsList, func(int) string) {
	job := pr(103, 101, "sh", "-c", "go test ./...")
	jobChild := pr(104, 103, "/usr/local/go/bin/go", "test", "./...")
	jobChild.PGID = 103
	snap := fake(
		pr(1, 0, "/sbin/launchd"),
		pr(50, 1, "/usr/local/bin/dun", "dund", "serve"),
		pr(100, 50, "/usr/local/bin/dun", "--engine", "--workspace", "/w/a"),
		pr(101, 100, "/usr/local/bin/mcpshell", "mcp", "--files-dir", "/w/a"),
		pr(102, 100, "/usr/local/bin/poly-lsp-mcp", "mcp", "--root", "/w/a", "--daemon"),
		job, jobChild,
		pr(105, 100, "/usr/local/bin/poly-lsp-mcp", "mcp", "--root=/w/a/.dun/wt/2", "--daemon"),
		pr(106, 100, "/opt/node/bin/node", "/opt/lib/github-mcp/index.js", "--stdio"),
		pr(107, 101, "/usr/bin/rg", "foo"),
		pr(150, 50, "/usr/local/bin/dun", "--engine", "--workspace", "/w/other"),
		pr(200, 1, "/usr/local/bin/poly-lsp-mcp", "daemon", "--socket=/tmp/s"),
		pr(201, 200, "/go/bin/gopls"),
		pr(202, 200, "/go/bin/gopls"),
		pr(203, 200, "/opt/node/bin/node", "/opt/lib/typescript-language-server", "--stdio"),
		pr(204, 201, "/go/bin/gopls", "** telemetry **"),
		pr(300, 1, "/usr/local/bin/mcpshell", "mcp"),
		pr(301, 1, "/go/bin/gopls"),
		pr(302, 1, "/usr/bin/vim"),
		pr(303, 1, "/usr/local/bin/dun", "--engine", "--workspace", "/w/gone"),
		pr(400, 1, "/bin/bash"),
	)
	list := &proto.ProcsList{DundPID: 50, Sessions: []proto.ProcSession{{
		ID: "20261117-093015", Workspace: "/w/a", EnginePID: 100,
		Agents: []proto.ProcAgent{{ID: 2, Dir: "/w/a/.dun/wt/2"}},
		Jobs:   []proto.ProcJob{{ID: 3, Command: "go test ./...", PGID: 103}},
	}}}
	cwds := map[int]string{201: "/w/a", 202: "/w/b", 203: "/w/a/.dun/wt/2", 301: "/w/a"}
	return snap, list, func(pid int) string { return cwds[pid] }
}

func byPID(rows []Row) map[int]Row {
	m := map[int]Row{}
	for _, r := range rows {
		if _, dup := m[r.PID]; dup {
			panic("a pid in two rows")
		}
		m[r.PID] = r
	}
	return m
}

func TestAttribute(t *testing.T) {
	snap, list, cwd := machine()
	rows := byPID(Attribute(snap, list, cwd))
	const a = "20261117-093015"
	want := []struct {
		pid    int
		group  string
		job    string
		orphan bool
	}{
		{100, a, "engine", false},
		{101, a, "shell (mcpshell)", false},
		{102, a, "poly-lsp proxy", false},
		{103, a, "background job #3 `go test ./...`", false},
		{104, a, "job #3 › go", false},
		{105, a, "sub-agent #2 poly-lsp proxy", false},
		{106, a, "tool server index.js", false},
		{107, a, "shell › rg", false},
		{50, GroupService, "dund", false},
		{150, GroupService, "engine", false}, // another user's session
		{200, GroupShared, "poly-lsp daemon", false},
		{201, a, "lsp gopls /w/a", false},
		{202, GroupNone, "lsp gopls /w/b", false}, // the root no session owns
		{203, a, "lsp typescript-language-server /w/a/.dun/wt/2", false},
		{204, a, "lsp gopls › gopls", false},
		{300, GroupNone, "orphaned shell (mcpshell)", true},
		{301, GroupNone, "orphaned lsp gopls /w/a", true},
		{303, GroupNone, "orphaned engine", true},
	}
	for _, w := range want {
		r, ok := rows[w.pid]
		if !ok {
			t.Errorf("pid %d: no row", w.pid)
			continue
		}
		if r.Group != w.group || r.Job != w.job || r.Orphan != w.orphan {
			t.Errorf("pid %d: got %q %q orphan=%v, want %q %q orphan=%v", w.pid, r.Group, r.Job, r.Orphan, w.group, w.job, w.orphan)
		}
	}
	for _, pid := range []int{1, 302, 400} {
		if r, ok := rows[pid]; ok {
			t.Errorf("pid %d is not dun's: %+v", pid, r)
		}
	}
	if r := rows[100]; r.Session != "1117-0930 a" || r.AgeSec != 3600 {
		t.Errorf("engine row: %+v", r)
	}
	if len(rows) != len(want) {
		t.Errorf("%d rows, want %d", len(rows), len(want))
	}
}

// With dund down there are no sessions: the daemon's servers are all
// (none), and the orphans are still found.
func TestAttribute_NoDund(t *testing.T) {
	snap, _, cwd := machine()
	rows := byPID(Attribute(snap, nil, cwd))
	if r := rows[201]; r.Group != GroupNone {
		t.Errorf("gopls with no sessions: %+v", r)
	}
	if r := rows[300]; !r.Orphan {
		t.Errorf("orphan shell: %+v", r)
	}
	if _, ok := rows[100]; ok {
		t.Error("an engine under a live parent is not an orphan")
	}
}

func TestRole(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/x/dun", "--engine", "--workspace", "/w"}, roleEngine},
		{[]string{"dun", "-p", "task"}, roleOther},
		{[]string{"/x/dun", "dund", "serve"}, roleDund},
		{[]string{"/home/u/.local/bin/mcpshell", "mcp", "--files-dir", "/w"}, roleShell},
		{[]string{"poly-lsp-mcp", "daemon", "--socket=/s"}, rolePolyDaemon},
		{[]string{"poly-lsp-mcp-linux-amd64", "mcp", "--root", ".", "--daemon"}, rolePolyProxy},
		{[]string{"poly-lsp-mcp", "mcp", "--root", "."}, rolePolyOwn},
		{[]string{"raglit", "daemon", "--addr", "x"}, roleRaglitDaemon},
		{[]string{"raglit", "serve", "--project", "p"}, roleRaglitClient},
		{nil, roleOther},
	} {
		if got := Role(c.argv); got != c.want {
			t.Errorf("Role(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
	if b := Base([]string{"/n/node", "--max-old-space-size=4096", "/l/typescript-language-server", "--stdio"}); b != "typescript-language-server" {
		t.Errorf("Base of a node script: %q", b)
	}
	if b := Base([]string{"python3", "-m", "pylsp"}); b != "python3" {
		t.Errorf("Base of python -m: %q", b)
	}
}

func TestOwner_DeepestTree(t *testing.T) {
	ss := []proto.ProcSession{
		{ID: "outer", Workspace: "/w"},
		{ID: "inner", Workspace: "/w/a"},
		{ID: "agent", Workspace: "/x", Agents: []proto.ProcAgent{{ID: 1, Dir: "/w/a/wt"}}},
	}
	for root, want := range map[string]string{"/w/a/sub": "inner", "/w/b": "outer", "/w/a/wt": "agent", "/w/ab": "outer", "/z": ""} {
		s, ok := Owner(root, ss)
		if (want == "") == ok || s.ID != want {
			t.Errorf("Owner(%s) = %q %v, want %q", root, s.ID, ok, want)
		}
	}
}

func TestRates(t *testing.T) {
	a := fake(&Proc{PID: 1, CPU: 10, Read: 0, Write: 0}, &Proc{PID: 2, CPU: 5, Read: -1, Write: -1}, &Proc{PID: 3, CPU: 1})
	b := fake(&Proc{PID: 1, CPU: 10.5, Read: 2048, Write: 1024}, &Proc{PID: 2, CPU: 6, Read: -1, Write: -1},
		&Proc{PID: 3, CPU: 0, Start: t0}) // pid 3 was reused
	b.At = a.At.Add(time.Second)
	r := Rates(a, b)
	if r[1].CPU != 50 || r[1].ReadBps != 2048 || r[1].WritBps != 1024 {
		t.Errorf("pid 1: %+v", r[1])
	}
	if r[2].CPU != 100 || r[2].ReadBps != -1 {
		t.Errorf("pid 2: %+v", r[2])
	}
	if _, ok := r[3]; ok {
		t.Error("a reused pid has no rate")
	}
}

func TestShortID(t *testing.T) {
	for id, want := range map[string]string{"20261021-222836": "1021-2228", "abcdef0123456789": "abcdef01", "s1": "s1"} {
		if got := ShortID(id); got != want {
			t.Errorf("ShortID(%s) = %s, want %s", id, got, want)
		}
	}
}

func sampleGroups() []Group {
	snap, list, cwd := machine()
	rows := Attribute(snap, list, cwd)
	for i := range rows {
		rows[i].CPU, rows[i].ReadBps, rows[i].WritBps = 0, 0, 0
		if rows[i].PID == 202 {
			rows[i].CPU = 97.5 // the leak
			rows[i].Job = "lsp gopls /Users/someone/src/very/long/path/to/a/repository/nobody/has/open/any/more"
		}
	}
	return Groups(rows)
}

// Groups sort by total CPU and rows by CPU; the hottest group leads.
func TestGroups_Order(t *testing.T) {
	gs := sampleGroups()
	if gs[0].Group != GroupNone || gs[0].Rows[0].PID != 202 || gs[0].CPU != 97.5 {
		t.Fatalf("first group: %+v", gs[0])
	}
}

// Every line fits the width, the columns line up, and only JOB is cut.
func TestTable_Widths(t *testing.T) {
	gs := sampleGroups()
	for _, width := range []int{60, 80, 120} {
		out := Table(gs, width)
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		col := strings.Index(lines[0], "CPU%")
		for _, ln := range lines {
			if n := len([]rune(ln)); n > width {
				t.Errorf("width %d: line of %d: %q", width, n, ln)
			}
			if ln == "" {
				continue
			}
			// CPU% is right-aligned in its column: its last digit under the %.
			if r := []rune(ln); len(r) > col+3 && r[col+3] == ' ' {
				t.Errorf("width %d: CPU column off: %q", width, ln)
			}
		}
		if width == 60 && !strings.Contains(out, "…") {
			t.Errorf("width 60: nothing cut:\n%s", out)
		}
	}
	// A cut root keeps its end, which names the tree.
	if out := Table(gs, 80); !strings.Contains(out, "…") || !strings.Contains(out, "any/more") {
		t.Errorf("cut root lost its end:\n%s", out)
	}
	// No width: not cut below maxJob.
	if out := Table(gs, 0); !strings.Contains(out, "lsp typescript-language-server /w/a/.dun/wt/2") {
		t.Errorf("width 0 cut a short job:\n%s", out)
	}
}

func TestSizeAge(t *testing.T) {
	for n, want := range map[float64]string{0: "0", 812: "812B", 4200: "4.1K", 412 << 20: "412M", 1.5 * (1 << 30): "1.5G"} {
		if got := Size(n); got != want {
			t.Errorf("Size(%v) = %s, want %s", n, got, want)
		}
	}
	for s, want := range map[int64]string{-1: "-", 45: "45s", 720: "12m", 3*3600 + 240: "3h04m", 2*86400 + 3*3600: "2d03h"} {
		if got := Age(s); got != want {
			t.Errorf("Age(%d) = %s, want %s", s, got, want)
		}
	}
}

// A sub-agent on a model tier is named with it: on the rows of its own
// tree, and in the session's label, a tiered child in its parent's tree
// included (it has no rows of its own). A child with no dir matches no argv.
func TestAttribute_AgentTiers(t *testing.T) {
	snap, list, cwd := machine()
	list.Sessions[0].Agents = []proto.ProcAgent{{ID: 2, Dir: "/w/a/.dun/wt/2", Tier: "fast"}, {ID: 4, Tier: "think"}}
	rows := byPID(Attribute(snap, list, cwd))
	if got := rows[105].Job; got != "sub-agent #2 (fast) poly-lsp proxy" {
		t.Errorf("job %q", got)
	}
	if got := rows[100].Session; !strings.Contains(got, "(sub-agents: #2 fast, #4 think)") {
		t.Errorf("session label %q", got)
	}
	if got := rows[101].Job; got != "shell (mcpshell)" {
		t.Errorf("an empty dir matched an argv: %q", got)
	}
}
