package procps

import (
	"fmt"
	"sort"
	"strings"
)

// Group is one session's rows (or one of the Group constants'), with
// their totals.
type Group struct {
	Group   string  `json:"group"`
	Session string  `json:"session"`
	Rows    []Row   `json:"rows"`
	CPU     float64 `json:"cpu"`
	RSS     int64   `json:"rss"`
	ReadBps float64 `json:"read_bps"` // -1 when no row's IO is known
	WritBps float64 `json:"write_bps"`
}

// Groups gathers rows by group: each group's rows by CPU, most first, and
// the groups by their total CPU, most first.
func Groups(rows []Row) []Group {
	idx := map[string]int{}
	var gs []Group
	for _, r := range rows {
		i, ok := idx[r.Group]
		if !ok {
			i = len(gs)
			idx[r.Group] = i
			gs = append(gs, Group{Group: r.Group, Session: r.Session, ReadBps: -1, WritBps: -1})
		}
		g := &gs[i]
		g.Rows = append(g.Rows, r)
		g.CPU += max(0, r.CPU)
		g.RSS += r.RSS
		if r.ReadBps >= 0 {
			g.ReadBps = max(0, g.ReadBps) + r.ReadBps
			g.WritBps = max(0, g.WritBps) + r.WritBps
		}
	}
	for i := range gs {
		sort.SliceStable(gs[i].Rows, func(a, b int) bool {
			ra, rb := gs[i].Rows[a], gs[i].Rows[b]
			if ra.CPU != rb.CPU {
				return ra.CPU > rb.CPU
			}
			return ra.PID < rb.PID
		})
	}
	sort.SliceStable(gs, func(a, b int) bool {
		if gs[a].CPU != gs[b].CPU {
			return gs[a].CPU > gs[b].CPU
		}
		return gs[a].Group < gs[b].Group
	})
	return gs
}

// Column widths other than SESSION's and JOB's.
const (
	wPID = 7
	wCPU = 5
	wMEM = 6
	wIO  = 11
	wAGE = 6
	gap  = 2
	// maxSession caps the SESSION column.
	maxSession = 22
	minJob     = 10
	// maxJob caps the JOB column when the terminal is wider (or unknown):
	// past it the other columns drift too far from the name to read across.
	maxJob = 64
	// maxRootJob is the same for a daemon's root lines, which say more.
	maxRootJob = 110
)

// Table is the groups as aligned text, width columns wide at most (0: no
// limit). Only the JOB column is cut to fit; a line never wraps.
func Table(gs []Group, width int) string {
	head, blocks := TableBlocks(gs, width)
	var b strings.Builder
	b.WriteString(head)
	for _, bl := range blocks {
		b.WriteString(bl.Text)
	}
	return b.String()
}

// Block is a part of the table: a process's lines (its own, then its
// roots, indexes and notes; PID set), a group's total line (PID 0, Group
// set) or the blank line between two groups (PID 0, Group empty).
type Block struct {
	PID   int
	Group string
	Text  string // each line ends in a newline
}

// TableBlocks is Table in parts: the header line and the blocks, which
// joined are Table's text. Columns align across the blocks.
func TableBlocks(gs []Group, width int) (string, []Block) {
	wSes := len("SESSION")
	wJob := len("JOB")
	wRoot := 0
	for _, g := range gs {
		wSes = max(wSes, runeLen(g.Session))
		for _, r := range g.Rows {
			wJob = max(wJob, runeLen(r.Job))
			names := RootNames(r.Roots)
			for i, root := range r.Roots {
				wSes = max(wSes, runeLen(root.Session))
				wRoot = max(wRoot, runeLen("  "+RootLine(root, names[i])))
			}
			for _, l := range RagLines(r) {
				wSes = max(wSes, runeLen(l[0]))
				wRoot = max(wRoot, runeLen(l[1]))
			}
		}
		wJob = max(wJob, runeLen(totalLabel(g)))
	}
	wSes = min(wSes, maxSession)
	wJob = max(min(wJob, maxJob), min(wRoot, maxRootJob))
	if width > 0 {
		fixed := wPID + wSes + wCPU + wMEM + wIO + wAGE + 6*gap
		wJob = max(minJob, min(wJob, width-fixed))
	}
	var b strings.Builder
	var blocks []Block
	flush := func(pid int, group string) {
		blocks = append(blocks, Block{PID: pid, Group: group, Text: b.String()})
		b.Reset()
	}
	line := func(pid, ses, job, cpu, mem, io, age string) {
		s := pad(pid, wPID) + sp() + pad(Fit(ses, wSes), wSes) + sp() + pad(FitJob(job, wJob), wJob) + sp() +
			lpad(cpu, wCPU) + sp() + lpad(mem, wMEM) + sp() + lpad(io, wIO) + sp() + lpad(age, wAGE)
		s = strings.TrimRight(s, " ")
		if width > 0 {
			s = Fit(s, width)
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	line("PID", "SESSION", "JOB", "CPU%", "MEM", "IO R/W /s", "AGE")
	head := b.String()
	b.Reset()
	for i, g := range gs {
		if i > 0 {
			b.WriteByte('\n')
			flush(0, "")
		}
		for _, r := range g.Rows {
			line(fmt.Sprint(r.PID), g.Session, r.Job, CPUText(r.CPU), Size(float64(r.RSS)), IOText(r.ReadBps, r.WritBps), Age(r.AgeSec))
			names := RootNames(r.Roots)
			for i, root := range r.Roots {
				line("", root.Session, "  "+RootLine(root, names[i]), "", "", "", "")
			}
			for _, l := range RagLines(r) {
				line("", l[0], l[1], "", "", "", "")
			}
			notes := r.RagNote
			if r.RootsErr != "" {
				notes = append([]string{r.RootsErr}, notes...)
			}
			for _, n := range notes {
				msg := "  " + n
				if width > 0 {
					msg = "\x1b[2m" + Fit(msg, width) + "\x1b[0m"
				}
				b.WriteString(msg + "\n")
			}
			flush(r.PID, g.Group)
		}
		if len(g.Rows) > 1 {
			line("", g.Session, totalLabel(g), CPUText(g.CPU), Size(float64(g.RSS)), IOText(g.ReadBps, g.WritBps), "")
			flush(0, g.Group)
		}
	}
	return head, blocks
}

func totalLabel(g Group) string { return fmt.Sprintf("= total, %d processes", len(g.Rows)) }

func sp() string { return strings.Repeat(" ", gap) }

// CPUText is a CPU percent as the table shows it; "-" before a rate.
func CPUText(c float64) string {
	if c < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", c)
}

// IOText is read/write per second; "-" unknown.
func IOText(r, w float64) string {
	if r < 0 || w < 0 {
		return "-"
	}
	return Size(r) + "/" + Size(w)
}

// Size is a byte count the short way: 0, 812B, 4.1K, 412M, 1.2G.
func Size(n float64) string {
	switch {
	case n < 0.5:
		return "0"
	case n < 1024:
		return fmt.Sprintf("%.0fB", n)
	case n < 10*1024:
		return fmt.Sprintf("%.1fK", n/1024)
	case n < 1024*1024:
		return fmt.Sprintf("%.0fK", n/1024)
	case n < 10*1024*1024:
		return fmt.Sprintf("%.1fM", n/(1024*1024))
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.0fM", n/(1024*1024))
	}
	return fmt.Sprintf("%.1fG", n/(1024*1024*1024))
}

// Age is seconds the short way: 45s, 12m, 3h04m, 2d03h; "-" unknown.
func Age(sec int64) string {
	switch {
	case sec < 0:
		return "-"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	}
	return fmt.Sprintf("%dd%02dh", sec/86400, sec%86400/3600)
}

func runeLen(s string) int { return len([]rune(s)) }

// Fit cuts s to n runes, the last one an ellipsis when cut.
func Fit(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:max(0, n)])
	}
	return string(r[:n-1]) + "…"
}

// FitJob is Fit, except a path in the job (an LSP's root) loses its
// middle, not its end: the end names the tree.
func FitJob(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	i := strings.LastIndex(s, " /")
	if j := strings.LastIndex(s, " ~"); j > i {
		i = j
	}
	if i < 0 {
		return Fit(s, n)
	}
	head := []rune(s[:i+1])
	path := []rune(s[i+1:])
	keep := n - len(head) - 1 // the ellipsis
	if keep < 8 {
		return Fit(s, n)
	}
	return string(head) + "…" + string(path[len(path)-keep:])
}

func pad(s string, n int) string {
	if k := runeLen(s); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}

func lpad(s string, n int) string {
	if k := runeLen(s); k < n {
		return strings.Repeat(" ", n-k) + s
	}
	return s
}
