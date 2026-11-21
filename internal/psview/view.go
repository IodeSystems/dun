package psview

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/procps"
	"github.com/iodesystems/dun/internal/tui"
)

// The screen: the title and the column names, the list, the details pane
// when open, then the footer (a status or y/n line, and the keys).
const (
	headRows = 2
	footRows = 2
	// maxDetails caps the details pane, its divider included.
	maxDetails = 14
)

// Column widths (as procps.Table's), and the sparkline's.
const (
	wGutter = 2
	wPID    = 7
	wCPU    = 5
	wSpark  = 10
	wMEM    = 6
	wIO     = 11
	wAGE    = 6
	gap     = 2
	minJob  = 16
	maxJob  = 64
)

// cols is which optional columns fit, and JOB's width.
type cols struct {
	spark, io, age bool
	job            int
}

// layout fits the columns to width: JOB takes what is left, up to maxJob;
// under minJob the sparkline goes first, then IO, then AGE. JOB is cut,
// never wrapped.
func layout(width int) cols {
	c := cols{spark: true, io: true, age: true}
	fixed := func() int {
		n := wGutter + wPID + gap + gap + wCPU + gap + wMEM
		if c.spark {
			n += gap + wSpark
		}
		if c.io {
			n += gap + wIO
		}
		if c.age {
			n += gap + wAGE
		}
		return n
	}
	for _, drop := range []*bool{&c.spark, &c.io, &c.age} {
		if width-fixed() >= minJob {
			break
		}
		*drop = false
	}
	c.job = max(1, min(maxJob, width-fixed()))
	return c
}

// line lays out one row's cells (gutter excluded), cut to width.
func (c cols) line(pid, job, cpu, spark, mem, io, age string, width int) string {
	head := lpad(pid, wPID) + sp() + pad(procps.FitJob(job, c.job), c.job)
	return procps.Fit(strings.TrimRight(head+c.tail(cpu, spark, mem, io, age), " "), max(0, width-wGutter))
}

// groupLine is a group's header: its label across PID and JOB, then its
// totals.
func (c cols) groupLine(label, cpu, mem, io string, width int) string {
	n := wPID + gap + c.job
	head := pad(procps.Fit(label, n), n)
	return procps.Fit(strings.TrimRight(head+c.tail(cpu, "", mem, io, ""), " "), max(0, width-wGutter))
}

func (c cols) tail(cpu, spark, mem, io, age string) string {
	s := sp() + lpad(cpu, wCPU)
	if c.spark {
		s += sp() + pad(spark, wSpark)
	}
	s += sp() + lpad(mem, wMEM)
	if c.io {
		s += sp() + lpad(io, wIO)
	}
	if c.age {
		s += sp() + lpad(age, wAGE)
	}
	return s
}

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// listH is the list's rows.
func (m *model) listH() int {
	return max(1, m.h-headRows-footRows-len(m.detailLines()))
}

// render is the whole screen, m.h lines, none wider than m.w.
func (m *model) render() string {
	w := max(1, m.w)
	c := layout(w)
	var out []string
	add := func(s string) { out = append(out, s) }

	// The title: the sort, the counts, the time.
	sorts := make([]string, nSorts)
	for i, n := range sortNames {
		if sortKey(i) == m.sort {
			sorts[i] = "[" + n + "]"
		} else {
			sorts[i] = n
		}
	}
	nproc := 0
	for _, g := range m.view.Groups {
		nproc += len(g.Rows)
	}
	title := "dun ps  sort " + strings.Join(sorts, " ")
	if m.have {
		title += fmt.Sprintf("  %s, %s  %s", plural(nproc, "process", "processes"),
			plural(m.view.Sessions, "session", "sessions"), m.view.At.Format("15:04:05"))
	}
	add(lipgloss.NewStyle().Bold(true).Render(procps.Fit(title, w)))
	hdr := map[sortKey]string{sortCPU: "CPU%", sortMEM: "MEM", sortIO: "IO R/W /s", sortAGE: "AGE"}
	hdr[m.sort] += "↓"
	add(tui.StDim.Render(pad("", wGutter) + c.line("PID", "JOB", hdr[sortCPU], "LAST MIN", hdr[sortMEM], hdr[sortIO], hdr[sortAGE], w)))

	// The list.
	its := m.items()
	n := m.listH()
	switch {
	case m.err != nil && !m.have:
		add(tui.StErr.Render(procps.Fit("  "+m.err.Error(), w)))
	case !m.have:
		add(tui.SecondaryStyle().Render(procps.Fit("  reading the process table…", w)))
	case len(its) == 0:
		add(tui.SecondaryStyle().Render(procps.Fit("  no process of dun's is running", w)))
	}
	for i := m.off; i < len(its) && i < m.off+n; i++ {
		add(m.itemLine(its[i], i == m.selIdx && its[i].key() == m.sel, c, w))
	}
	for len(out) < headRows+n {
		add("")
	}
	out = out[:headRows+n]

	out = append(out, m.detailLines()...)

	// The footer.
	var msg string
	switch {
	case m.ask != nil:
		msg = tui.NoteStyle().Bold(true).Render(procps.Fit(m.ask.prompt+" y/n", w))
	case m.status != "" && m.statusErr:
		msg = tui.StErr.Render(procps.Fit(m.status, w))
	case m.status != "":
		msg = tui.StTool.Render(procps.Fit(m.status, w))
	case m.err != nil:
		msg = tui.StErr.Render(procps.Fit("reading: "+m.err.Error(), w))
	case m.view.Note != "":
		msg = tui.SecondaryStyle().Render(procps.Fit(m.view.Note, w))
	}
	add(msg)
	add(tui.StDim.Render(procps.Fit(m.hints(), w)))
	for len(out) < m.h {
		add("")
	}
	if len(out) > m.h && m.h > 0 {
		out = out[:m.h]
	}
	return strings.Join(out, "\n")
}

func (m *model) hints() string {
	if m.ask != nil {
		return "y yes · n no"
	}
	h := "↑↓ move · enter fold/details · S sort · k kill · s stop · o open"
	if it, ok := m.selected(); ok && it.kind == kProc && isPolyDaemon(*it.row) {
		h += " · r restart"
	}
	if m.frozen != nil {
		h += " · sort paused, t re-sorts"
	}
	return h + " · q quit"
}

// itemLine is one list line, the selected one in the accent behind the
// gutter mark. Other lines keep their own colours (focus never dims).
func (m *model) itemLine(it item, sel bool, c cols, w int) string {
	var s string
	switch it.kind {
	case kGroup:
		g := it.group
		mark := "▾ "
		if m.folded[g.Group] {
			mark = "▸ "
		}
		label := mark + g.Session + "  " + plural(len(g.Rows), "process", "processes")
		s = c.groupLine(label, procps.CPUText(g.CPU), procps.Size(float64(g.RSS)), procps.IOText(g.ReadBps, g.WritBps), w)
		if sel {
			return tui.AccentStyle().Render("▎ " + s)
		}
		return "  " + lipgloss.NewStyle().Bold(true).Render(s)
	case kProc:
		r := it.row
		spark := ""
		if h := m.sparks[r.PID]; h != nil {
			spark = sparkline(h.r.values(), wSpark)
		}
		s = c.line(fmt.Sprint(r.PID), r.Job, procps.CPUText(r.CPU), spark, procps.Size(float64(r.RSS)),
			procps.IOText(r.ReadBps, r.WritBps), procps.Age(r.AgeSec), w)
		if sel {
			return tui.AccentStyle().Render("▎ " + s)
		}
		return "  " + s
	case kRoot:
		ses := it.root.Session
		s = procps.Fit(pad("", wPID+gap)+"  ↳ "+ses+"  "+procps.RootLine(it.root, it.name), max(0, w-wGutter))
		return "  " + tui.SecondaryStyle().Render(s)
	case kRootErr:
		s = procps.Fit(pad("", wPID+gap)+"  "+it.row.RootsErr, max(0, w-wGutter))
		return "  " + tui.SecondaryStyle().Render(s)
	case kRagNote:
		s = procps.Fit(pad("", wPID+gap)+"  "+it.note, max(0, w-wGutter))
		return "  " + tui.SecondaryStyle().Render(s)
	case kRagEmbed:
		s = procps.Fit(pad("", wPID+gap)+"  "+procps.RagEmbedLine(*it.row.RagEmbed), max(0, w-wGutter))
		return "  " + tui.SecondaryStyle().Render(s)
	case kRag:
		s = procps.Fit(pad("", wPID+gap)+"  ↳ "+procps.RagSession(it.rag)+"  "+procps.RagLine(it.rag), max(0, w-wGutter))
		if sel {
			return tui.AccentStyle().Render("▎ " + s)
		}
		return "  " + tui.SecondaryStyle().Render(s)
	case kRagIdle:
		s = procps.Fit(pad("", wPID+gap)+"  "+procps.RagIdleLine(len(it.row.RagIdle)), max(0, w-wGutter))
		return "  " + tui.SecondaryStyle().Render(s)
	}
	return ""
}

// detailLines is the details pane: a divider, then the selected process's
// argv, working directory, root(s) and parents. None when closed.
func (m *model) detailLines() []string {
	if !m.details {
		return nil
	}
	it, ok := m.selected()
	if !ok || it.kind != kProc && it.kind != kRag {
		return nil
	}
	if it.kind == kRag {
		return m.ragDetails(it)
	}
	w := max(1, m.w)
	r := it.row
	var body []string
	kv := func(k, v string) {
		body = append(body, procps.Fit(pad(k, 9)+v, w))
	}
	kv("pid", fmt.Sprintf("%d  ppid %d  session %s  age %s", r.PID, r.PPID, r.Session, procps.Age(r.AgeSec)))
	// argv wraps, up to three lines: it is what the row cuts.
	argv := []rune(r.Argv)
	room := max(1, w-9)
	for i := 0; i < 3 && len(argv) > 0; i++ {
		k := ""
		if i == 0 {
			k = "argv"
		}
		n := min(room, len(argv))
		chunk := string(argv[:n])
		argv = argv[n:]
		if i == 2 && len(argv) > 0 {
			chunk = procps.Fit(chunk+"…", room)
		}
		kv(k, chunk)
	}
	if m.detCwd != "" {
		kv("cwd", procps.Tilde(m.detCwd))
	}
	if r.Root != "" {
		kv("root", procps.Tilde(r.Root))
	}
	names := procps.RootNames(r.Roots)
	for i, root := range r.Roots {
		kv(map[bool]string{true: "roots", false: ""}[i == 0], root.Session+"  "+procps.RootLine(root, names[i]))
	}
	if r.RagEmbed != nil {
		kv("rag", fmt.Sprintf("%s  %s active, %d idle", procps.RagEmbedLine(*r.RagEmbed),
			plural(len(r.Rag), "index", "indexes"), len(r.RagIdle)))
	}
	if chain := m.parents(r.PPID); chain != "" {
		kv("parents", chain)
	}
	return m.pane("─ details "+fmt.Sprint(r.PID)+" ", body)
}

// pane is the details pane: the divider, labelled, then body cut to the
// pane's height.
func (m *model) pane(label string, body []string) []string {
	w := max(1, m.w)
	h := min(maxDetails, max(3, m.h/2))
	if len(body) > h-1 {
		body = append(body[:h-2], procps.Fit(fmt.Sprintf("… %d more lines", len(body)-(h-2)), w))
	}
	div := tui.StDim.Render(procps.Fit(label+strings.Repeat("─", max(0, w-len([]rune(label)))), w))
	return append([]string{div}, body...)
}

// ragDetails is the details pane of an index: its counters.
func (m *model) ragDetails(it item) []string {
	w := max(1, m.w)
	x := it.rag
	var body []string
	kv := func(k, v string) {
		body = append(body, procps.Fit(pad(k, 10)+v, w))
	}
	branch := ""
	if x.Branch != "" {
		branch = "  branch " + x.Branch
	}
	kv("index", x.Name+"  "+x.State+"  session "+procps.RagSession(x)+branch)
	queue := fmt.Sprint(x.Queue)
	if x.QueueCapped {
		queue += "+"
	}
	kv("queue", fmt.Sprintf("%s pending  %d running  %d embeds in flight", queue, x.Running, x.EmbedInFlight))
	kv("last min", fmt.Sprintf("%d files  %d chunks  %d embeds  %s embedding  ~%d tokens",
		x.FilesMin, x.ChunksMin, x.EmbedsMin, ms(x.EmbedMsMin), x.TokensMin))
	kv("total", fmt.Sprintf("%d files  %d chunks  %d embeds  %s embedding  ~%d tokens",
		x.Files, x.Chunks, x.Embeds, ms(x.EmbedMs), x.Tokens))
	if e := it.row.RagEmbed; e != nil {
		kv("embed", procps.RagEmbedLine(*e))
	}
	if x.IdleSec >= 0 {
		kv("active", procps.Age(x.IdleSec)+" ago")
	} else {
		kv("active", "not since dund started")
	}
	if x.LastErr != "" {
		kv("last err", procps.Age(x.ErrAgeSec)+" ago  "+x.LastErr)
	}
	return m.pane("─ rag "+x.Name+" ", body)
}

// ms is milliseconds the short way: 840ms, 12.4s, 3m.
func ms(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%dms", n)
	}
	if n < 60000 {
		return fmt.Sprintf("%.1fs", float64(n)/1000)
	}
	return procps.Age(n / 1000)
}

// parents is the chain from ppid up to pid 1, each "pid name".
func (m *model) parents(ppid int) string {
	var parts []string
	for pid, n := ppid, 0; pid > 0 && n < 12; n++ {
		p := m.snap.Procs[pid]
		if p == nil {
			parts = append(parts, fmt.Sprint(pid))
			break
		}
		parts = append(parts, fmt.Sprintf("%d %s", pid, procps.Base(p.Argv)))
		if pid == 1 {
			break
		}
		pid = p.PPID
	}
	return strings.Join(parts, " › ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func sp() string { return strings.Repeat(" ", gap) }

func pad(s string, n int) string {
	if k := len([]rune(s)); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}

func lpad(s string, n int) string {
	if k := len([]rune(s)); k < n {
		return strings.Repeat(" ", n-k) + s
	}
	return s
}
