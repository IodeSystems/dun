package psview

import (
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
)

// sortKey is what rows and groups are ordered by, most first.
type sortKey int

const (
	sortCPU sortKey = iota
	sortMEM
	sortIO
	sortAGE
	nSorts
)

var sortNames = [nSorts]string{"CPU", "MEM", "IO", "AGE"}

type itemKind int

const (
	kGroup   itemKind = iota // a session's header: fold, totals
	kProc                    // a process
	kRoot                    // a root the poly-lsp daemon holds
	kRootErr                 // why the daemon's roots are missing
)

// item is one line of the list.
type item struct {
	kind  itemKind
	group *procps.Group
	row   *procps.Row // kProc, kRoot, kRootErr: the process (the daemon for a root)
	root  procps.Root
	name  string // kRoot: the root's short name
}

func (it item) selectable() bool { return it.kind == kGroup || it.kind == kProc }

// key is an item's identity: rows are rebuilt on every reading, so the
// selection is kept by what it is, not where it was.
func (it item) key() selKey {
	if it.kind == kGroup {
		return selKey{group: it.group.Group}
	}
	return selKey{group: it.group.Group, pid: it.row.PID}
}

type selKey struct {
	group string
	pid   int // 0: the group's header
}

// confirm is a y/n question in the footer; yes is what y does.
type confirm struct {
	prompt string
	yes    func() tea.Cmd
}

type openReq struct{ id, workspace string }

// Messages.
type (
	sampleMsg struct {
		v    dundcli.PSView
		snap procps.Snapshot
		err  error
	}
	tickMsg   struct{}
	actionMsg struct {
		text   string
		err    error
		termed int // a pid SIGTERM was sent to
	}
)

type model struct {
	sample func() (dundcli.PSView, procps.Snapshot, error)
	act    Actions
	every  time.Duration
	cwd    func(int) string

	w, h    int
	view    dundcli.PSView
	snap    procps.Snapshot
	err     error
	have    bool // a reading has arrived
	sort    sortKey
	folded  map[string]bool
	sel     selKey
	selIdx  int // the selection's last index, for when it goes away
	off     int // the list's top item
	details bool
	detCwd  string // the selected process's working directory, read on open and each reading

	ask       *confirm
	status    string
	statusErr bool

	sparks   map[int]*sparkHist
	ringSize int
	// termed are pids sent SIGTERM, by the start they had: still there at
	// the next reading, the view offers SIGKILL.
	termed map[int]termedAt

	open *openReq
}

type sparkHist struct {
	start time.Time
	r     *ring
}

type termedAt struct {
	start time.Time
	at    time.Time
}

func newModel(sample func() (dundcli.PSView, procps.Snapshot, error), act Actions, every time.Duration, cwd func(int) string) *model {
	if every <= 0 {
		every = 2 * time.Second
	}
	if cwd == nil {
		cwd = func(int) string { return "" }
	}
	return &model{sample: sample, act: act, every: every, cwd: cwd, w: 80, h: 24,
		folded: map[string]bool{}, sparks: map[int]*sparkHist{}, ringSize: ringSize(every), termed: map[int]termedAt{}}
}

func (m *model) Init() tea.Cmd { return m.read() }

func (m *model) read() tea.Cmd {
	s := m.sample
	return func() tea.Msg {
		v, snap, err := s()
		return sampleMsg{v: v, snap: snap, err: err}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.follow()
	case sampleMsg:
		m.onSample(msg)
		return m, tea.Tick(m.every, func(time.Time) tea.Msg { return tickMsg{} })
	case tickMsg:
		return m, m.read()
	case actionMsg:
		m.status, m.statusErr = msg.text, msg.err != nil
		if msg.err != nil {
			m.status = msg.text + ": " + msg.err.Error()
		}
		if msg.termed != 0 {
			if p := m.snap.Procs[msg.termed]; p != nil {
				m.termed[msg.termed] = termedAt{start: p.Start, at: time.Now()}
			}
		}
	case tea.KeyPressMsg:
		return m, m.key(msg.String())
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			m.click(msg.Y)
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll(-3)
		case tea.MouseWheelDown:
			m.scroll(3)
		}
	}
	return m, nil
}

func (m *model) onSample(msg sampleMsg) {
	if msg.err != nil {
		m.err = msg.err
		return
	}
	m.err, m.have = nil, true
	m.view, m.snap = msg.v, msg.snap
	seen := map[int]bool{}
	for _, g := range m.view.Groups {
		for _, r := range g.Rows {
			seen[r.PID] = true
			p := m.snap.Procs[r.PID]
			var start time.Time
			if p != nil {
				start = p.Start
			}
			h := m.sparks[r.PID]
			if h == nil || !h.start.Equal(start) {
				h = &sparkHist{start: start, r: newRing(m.ringSize)}
				m.sparks[r.PID] = h
			}
			if r.CPU >= 0 {
				h.r.push(r.CPU)
			}
		}
	}
	for pid := range m.sparks {
		if !seen[pid] {
			delete(m.sparks, pid)
		}
	}
	m.reselect()
	if m.details {
		if it, ok := m.selected(); ok && it.kind == kProc {
			m.detCwd = m.cwd(it.row.PID)
		}
	}
	m.checkTermed()
}

// checkTermed offers SIGKILL for a process still running a reading after
// its SIGTERM.
func (m *model) checkTermed() {
	for pid, t := range m.termed {
		if m.view.At.Before(t.at.Add(time.Second)) {
			continue
		}
		delete(m.termed, pid)
		p := m.snap.Procs[pid]
		if p == nil || !p.Start.Equal(t.start) || m.ask != nil {
			continue
		}
		m.ask = &confirm{
			prompt: fmt.Sprintf("%d is still running after SIGTERM: send SIGKILL?", pid),
			yes:    m.killCmd(pid, true),
		}
	}
}

// groups are the reading's groups in the current sort: rows within each,
// then the groups by their totals.
func (m *model) groups() []procps.Group {
	gs := make([]procps.Group, len(m.view.Groups))
	copy(gs, m.view.Groups)
	for i := range gs {
		rows := append([]procps.Row(nil), gs[i].Rows...)
		sort.SliceStable(rows, func(a, b int) bool {
			va, vb := m.rowVal(rows[a]), m.rowVal(rows[b])
			if va != vb {
				return va > vb
			}
			return rows[a].PID < rows[b].PID
		})
		gs[i].Rows = rows
	}
	sort.SliceStable(gs, func(a, b int) bool {
		va, vb := m.groupVal(gs[a]), m.groupVal(gs[b])
		if va != vb {
			return va > vb
		}
		return gs[a].Group < gs[b].Group
	})
	return gs
}

func (m *model) rowVal(r procps.Row) float64 {
	switch m.sort {
	case sortMEM:
		return float64(r.RSS)
	case sortIO:
		return max(0, r.ReadBps) + max(0, r.WritBps)
	case sortAGE:
		return float64(r.AgeSec)
	}
	return r.CPU
}

// groupVal is a group's total, or for AGE its oldest process.
func (m *model) groupVal(g procps.Group) float64 {
	switch m.sort {
	case sortMEM:
		return float64(g.RSS)
	case sortIO:
		return max(0, g.ReadBps) + max(0, g.WritBps)
	case sortAGE:
		old := -1.0
		for _, r := range g.Rows {
			old = max(old, float64(r.AgeSec))
		}
		return old
	}
	return g.CPU
}

// items are the list's lines, folded groups showing their header only.
func (m *model) items() []item {
	var out []item
	gs := m.groups()
	for gi := range gs {
		g := &gs[gi]
		out = append(out, item{kind: kGroup, group: g})
		if m.folded[g.Group] {
			continue
		}
		for ri := range g.Rows {
			r := &g.Rows[ri]
			out = append(out, item{kind: kProc, group: g, row: r})
			names := procps.RootNames(r.Roots)
			for i, root := range r.Roots {
				out = append(out, item{kind: kRoot, group: g, row: r, root: root, name: names[i]})
			}
			if r.RootsErr != "" {
				out = append(out, item{kind: kRootErr, group: g, row: r})
			}
		}
	}
	return out
}

func (m *model) selected() (item, bool) {
	its := m.items()
	for _, it := range its {
		if it.selectable() && it.key() == m.sel {
			return it, true
		}
	}
	return item{}, false
}

// reselect keeps the selection on its item, or, when that is gone, on the
// nearest selectable line at its last place.
func (m *model) reselect() {
	its := m.items()
	if len(its) == 0 {
		return
	}
	for i, it := range its {
		if it.selectable() && it.key() == m.sel {
			m.selIdx = i
			m.follow()
			return
		}
	}
	i := min(m.selIdx, len(its)-1)
	for j := i; j >= 0; j-- {
		if its[j].selectable() {
			m.sel, m.selIdx = its[j].key(), j
			break
		}
	}
	m.follow()
}

// move steps the selection by d selectable lines.
func (m *model) move(d int) {
	its := m.items()
	i := m.selIdx
	for steps := abs(d); steps > 0; {
		j := i + sign(d)
		for j >= 0 && j < len(its) && !its[j].selectable() {
			j += sign(d)
		}
		if j < 0 || j >= len(its) {
			break
		}
		i = j
		steps--
	}
	if i >= 0 && i < len(its) && its[i].selectable() {
		m.sel, m.selIdx = its[i].key(), i
	}
	m.follow()
}

func (m *model) moveTo(i int) {
	its := m.items()
	if i < 0 || i >= len(its) {
		return
	}
	m.selIdx = i
	m.move(0)
	if !its[i].selectable() {
		m.move(-1)
	}
}

// follow scrolls the list so the selection is on screen.
func (m *model) follow() {
	n := m.listH()
	if m.selIdx < m.off {
		m.off = m.selIdx
	}
	if m.selIdx >= m.off+n {
		m.off = m.selIdx - n + 1
	}
	m.clampOff()
}

func (m *model) clampOff() {
	m.off = max(0, min(m.off, len(m.items())-m.listH()))
}

// scroll moves the view (the wheel), the selection kept on screen.
func (m *model) scroll(d int) {
	m.off += d
	m.clampOff()
	n := m.listH()
	if m.selIdx < m.off {
		m.moveTo(m.off)
	} else if m.selIdx >= m.off+n {
		m.moveTo(m.off + n - 1)
	}
}

// click selects the line at screen row y; on a group's header it also
// folds or opens the group.
func (m *model) click(y int) {
	i := m.off + y - headRows
	if y < headRows || y >= headRows+m.listH() || i >= len(m.items()) {
		return
	}
	its := m.items()
	if !its[i].selectable() {
		return
	}
	m.sel, m.selIdx = its[i].key(), i
	if its[i].kind == kGroup {
		m.toggle(its[i].group.Group)
	}
	m.follow()
}

func (m *model) toggle(group string) {
	m.folded[group] = !m.folded[group]
	m.reselect()
}

func (m *model) key(k string) tea.Cmd {
	if m.ask != nil {
		switch k {
		case "y", "Y":
			a := m.ask
			m.ask = nil
			return a.yes()
		case "n", "N", "esc":
			m.ask = nil
			m.status = "cancelled"
			m.statusErr = false
		case "ctrl+c":
			return tea.Quit
		}
		return nil
	}
	m.status = ""
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "esc":
		if m.details {
			m.details = false
			return nil
		}
		return tea.Quit
	case "up":
		m.move(-1)
	case "down":
		m.move(1)
	case "pgup":
		m.move(-max(1, m.listH()-1))
	case "pgdown":
		m.move(max(1, m.listH()-1))
	case "home":
		m.moveTo(0)
	case "end":
		m.moveTo(len(m.items()) - 1)
	case "S", "shift+s":
		m.sort = (m.sort + 1) % nSorts
		m.reselect()
	case "enter", "space":
		it, ok := m.selected()
		switch {
		case !ok:
		case it.kind == kGroup:
			m.toggle(it.group.Group)
		case k == "enter":
			m.details = !m.details
			if m.details {
				m.detCwd = m.cwd(it.row.PID)
			}
			m.follow()
		}
	case "k":
		it, ok := m.selected()
		if !ok || it.kind != kProc {
			m.say("k kills a process: select its row", true)
			return nil
		}
		m.ask = &confirm{
			prompt: fmt.Sprintf("send SIGTERM to %d %s?", it.row.PID, it.row.Job),
			yes:    m.killCmd(it.row.PID, false),
		}
	case "s":
		id, ok := m.sessionOf()
		if !ok {
			return nil
		}
		act := m.act.Stop
		m.ask = &confirm{
			prompt: fmt.Sprintf("stop session %s?", procps.ShortID(id)),
			yes: func() tea.Cmd {
				return func() tea.Msg {
					text := "stopped session " + procps.ShortID(id)
					if act == nil {
						return actionMsg{text: "stop session", err: fmt.Errorf("no service")}
					}
					if err := act(id); err != nil {
						return actionMsg{text: "stop session " + procps.ShortID(id), err: err}
					}
					return actionMsg{text: text}
				}
			},
		}
	case "r":
		it, ok := m.selected()
		if !ok || it.kind != kProc || !isPolyDaemon(*it.row) {
			m.say("r restarts the poly-lsp daemon: select its row", true)
			return nil
		}
		var argv []string
		if p := m.snap.Procs[it.row.PID]; p != nil {
			argv = p.Argv
		}
		act := m.act.Restart
		m.ask = &confirm{
			prompt: "restart the poly-lsp daemon (poly-lsp-mcp daemon --restart)?",
			yes: func() tea.Cmd {
				m.say("restarting the poly-lsp daemon…", false)
				return func() tea.Msg {
					if act == nil {
						return actionMsg{text: "restart", err: fmt.Errorf("not available")}
					}
					if err := act(argv); err != nil {
						return actionMsg{text: "restart the poly-lsp daemon", err: err}
					}
					return actionMsg{text: "restarted the poly-lsp daemon"}
				}
			},
		}
	case "o":
		id, ok := m.sessionOf()
		if !ok {
			return nil
		}
		ws := ""
		if m.view.List != nil {
			for _, s := range m.view.List.Sessions {
				if s.ID == id {
					ws = s.Workspace
				}
			}
		}
		m.open = &openReq{id: id, workspace: ws}
		return tea.Quit
	}
	return nil
}

func (m *model) killCmd(pid int, force bool) func() tea.Cmd {
	act := m.act.Kill
	return func() tea.Cmd {
		return func() tea.Msg {
			sig := "SIGTERM"
			if force {
				sig = "SIGKILL"
			}
			text := fmt.Sprintf("%s sent to %d", sig, pid)
			if act == nil {
				return actionMsg{text: text, err: fmt.Errorf("not available")}
			}
			if err := act(pid, force); err != nil {
				return actionMsg{text: fmt.Sprintf("%s to %d", sig, pid), err: err}
			}
			msg := actionMsg{text: text}
			if !force {
				msg.termed = pid
			}
			return msg
		}
	}
}

// sessionOf is the selected line's session id, or says why there is none.
func (m *model) sessionOf() (string, bool) {
	it, ok := m.selected()
	if !ok {
		return "", false
	}
	switch it.group.Group {
	case procps.GroupNone, procps.GroupShared, procps.GroupService:
		m.say(it.group.Group+" is not a session: select a session's row", true)
		return "", false
	}
	return it.group.Group, true
}

func (m *model) say(s string, isErr bool) { m.status, m.statusErr = s, isErr }

func isPolyDaemon(r procps.Row) bool {
	return r.Group == procps.GroupShared && r.Job == "poly-lsp daemon"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
