package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"
)

// The job view: what → (or enter) on a job in the activity zone opens, in the
// conversation pane's place. Like the pager, it takes that pane and nothing
// else — the header, the strip, the composer and the status line stay.
//
//	✗ job 3 · exit 2 · 1m12s · 4,210 bytes          120–140 of 140
//	▎▾ $ go test -race ./...
//	      the whole command, wrapped, when it does not fit on the head
//	 ▾ output  ↑ 120 more · following
//	   the newest rows of the log
//
// It is laid out the way a run of tool calls is in the pager (USER
// 2026-10-23: "similar to multistep tool+think"): items with heads, ▾/▸ for
// open and shut, a gutter on the selected one. The command is the top item
// and opens by default; enter, → and ← open and shut it. The output is a tail
// panel: it follows the log while the job runs; ↑/↓, pgup/pgdn and g/G scroll
// it, and scrolling back to the end follows again. / and ? search it, n/N step
// through the matches — the inspector's keys. ← on the output, q or esc comes
// back out.
//
// It replaced the inspector for jobs: the inspector reads a snapshot, and a
// running job is exactly the thing whose log keeps growing.
//
// The log is the file the engine writes and names in the jobs event (bgjob.go
// openLogLocked) — the one exec_monitor reads. It is read here in increments
// on a timer while the view is open (jobTailEvery), never round-tripped
// through the engine.

// jobTailEvery is how often an open job view reads the log for new output.
const jobTailEvery = 500 * time.Millisecond

// jobTabStop: a tab in a log draws as spaces to the next multiple of this, as
// the terminal the command was written for would show it.
const jobTabStop = 8

type jobView struct {
	id      int
	cmdOpen bool // the command item is open (the default)
	onCmd   bool // the selection is on the command item, not the output

	// The output panel's window.
	off    int
	follow bool // pinned to the newest row: new output scrolls the panel
	shown  int  // panel rows, as of the last layout
	maxOff int

	// The log as read so far.
	path string
	data []byte
	read int64 // bytes of the file consumed
	cut  bool  // the front was dropped: only the last jobLogTail is kept
	err  string
	ver  int // bumped when data changes

	// The output wrapped to the panel's width, by (ver, w). Rows of complete
	// lines are kept across appends (stable bytes of data → stableRows rows),
	// so a growing log costs its new lines, not the whole tail.
	rows       []string
	rowsVer    int
	rowsW      int
	stable     int
	stableRows int

	// Search, as in the inspector: /, ?, n, N.
	search    textinput.Model
	searching bool
	query     string
	dir       int
	matches   []int // rows containing the query
	at        int

	lines []string // the pane, as of the last layout
	w, h  int
}

func newJobView(id int) jobView {
	se := textinput.New()
	se.Prompt = "/"
	return jobView{id: id, cmdOpen: true, follow: true, dir: 1, search: se, rowsVer: -1}
}

// load reads whatever the log has gained since the last read. A file that got
// shorter was replaced; it is read again from the start.
func (v *jobView) load(path string) {
	if path != v.path {
		v.path, v.cut, v.err = path, false, ""
		v.replace(nil, 0)
	}
	if path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		v.fail(err)
		return
	}
	size := st.Size()
	if size < v.read {
		v.cut = false
		v.replace(nil, 0)
	}
	if size == v.read && v.err == "" {
		return
	}
	from := v.read
	if size-from > jobLogTail { // more than the tail holds: skip to it
		from, v.cut = size-jobLogTail, true
		v.replace(nil, from)
	}
	f, err := os.Open(path)
	if err != nil {
		v.fail(err)
		return
	}
	defer f.Close()
	buf, err := io.ReadAll(io.NewSectionReader(f, from, size-from))
	if err != nil && len(buf) == 0 {
		v.fail(err)
		return
	}
	v.err = ""
	v.data = append(v.data, buf...)
	v.read = from + int64(len(buf))
	v.ver++
	// Past the cap, drop a quarter at a time at a line boundary, so a log
	// growing past it re-wraps every few reads rather than on every one.
	if len(v.data) > jobLogTail {
		drop := len(v.data) - jobLogTail*3/4
		if i := strings.IndexByte(string(v.data[drop:]), '\n'); i >= 0 {
			drop += i + 1
		}
		v.cut = true
		v.replace(append([]byte(nil), v.data[drop:]...), v.read)
	}
}

// replace swaps the data for data, read through byte read of the file; the
// wrapped rows no longer line up with it.
func (v *jobView) replace(data []byte, read int64) {
	v.data, v.read = data, read
	v.stable, v.stableRows, v.rows = 0, 0, v.rows[:0]
	v.ver++
}

func (v *jobView) fail(err error) {
	if v.err != err.Error() {
		v.err = err.Error()
		v.ver++
	}
}

// logLine is one line of a log as it would look on a terminal: after the last
// carriage return (a progress bar redrawing itself), without escape codes
// (colour, cursor motion — the panel is plain text, and search reads it), and
// with tabs drawn to their stops.
func logLine(s string) string {
	s = strings.TrimSuffix(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = stripANSI(s)
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := jobTabStop - col%jobTabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += lipgloss.Width(string(r))
	}
	return b.String()
}

// wrap brings v.rows up to date with the log at width w.
func (v *jobView) wrap(w int) {
	if v.rowsVer == v.ver && v.rowsW == w {
		return
	}
	if v.rowsW != w {
		v.stable, v.stableRows, v.rows = 0, 0, v.rows[:0]
	}
	v.rowsVer, v.rowsW = v.ver, w
	v.rows = v.rows[:v.stableRows]
	rest := string(v.data[v.stable:])
	lines := strings.Split(rest, "\n")
	for k, ln := range lines {
		if k == len(lines)-1 && ln == "" {
			break // the newline that ended the last line starts no row
		}
		v.rows = append(v.rows, strings.Split(cellbuf.Wrap(logLine(ln), max(1, w), ""), "\n")...)
		if k < len(lines)-1 { // a finished line: its rows will not change
			v.stable += len(ln) + 1
			v.stableRows = len(v.rows)
		}
	}
	if v.query != "" {
		v.findMatches()
	}
}

// jobStatus is the title's verdict: running, the exit code, or how it ended.
func jobStatus(j jobRow) string {
	switch j.state {
	case "running":
		return "running"
	case "ok":
		return "exit 0"
	case "failed":
		return fmt.Sprintf("exit %d", j.code)
	case "killed":
		return "killed"
	case "timeout":
		return "timed out"
	}
	return j.state
}

// layout lays the view out for job j at w×h (h includes the title row).
func (v *jobView) layout(j jobRow, w, h int, now time.Time) {
	v.w, v.h = max(1, w), max(4, h)
	row := jobActRow(j, now)
	title := fmt.Sprintf("%s job %d · %s · %s", row.mark, j.id, jobStatus(j), compactDur(j.elapsed(now)))
	if j.bytes > 0 {
		title += commaf(" · %d bytes", j.bytes)
	}
	if j.muted {
		title += " · muted"
	}

	// The command: the top item.
	gutter := func(sel bool) string {
		if sel {
			return stSel.Render("▎")
		}
		return " "
	}
	mark := func(open bool) string {
		if open {
			return stDim.Render("▾ ")
		}
		return stDim.Render("▸ ")
	}
	cmd := strings.TrimSpace(j.command)
	// A command that fits on its head is all head; one that does not opens
	// to the whole of it, and the head is then just the item's name.
	headText, cut := fitPlain("$ "+oneLine(cmd), v.w-3)
	long := cut || strings.Contains(cmd, "\n")
	if v.cmdOpen && long {
		headText = "command"
	}
	cmdRows := []string{gutter(v.onCmd) + mark(v.cmdOpen) + stTool.Render(headText)}
	if v.cmdOpen && long {
		body := strings.Split(cellbuf.Wrap("$ "+cmd, max(1, v.w-3), ""), "\n")
		// The command never takes the panel's place: a third of the pane at
		// most, and it says what it held back.
		limit := max(1, (v.h-1)/3)
		if len(body) > limit {
			more := len(body) - limit + 1
			body = append(body[:limit-1], stDim.Render(fmt.Sprintf("… %d more rows", more)))
		}
		for _, r := range body {
			cmdRows = append(cmdRows, "   "+r)
		}
	}

	footer := ""
	switch {
	case v.searching:
		footer = v.search.View()
	case len(v.matches) > 0:
		footer = stDim.Render(fmt.Sprintf("match %d/%d  ·  n/N next/prev · esc clear", v.at+1, len(v.matches)))
	case v.query != "":
		footer = stDim.Render(fmt.Sprintf("no match for %q · esc clear", v.query))
	}
	panelH := v.h - 1 - len(cmdRows) - 1
	if footer != "" {
		panelH--
	}
	v.shown = max(1, panelH)

	// The output: a tail panel.
	v.wrap(max(1, v.w-3))
	rows := v.rows
	var note string
	switch {
	case v.err != "":
		note = "(log unreadable: " + v.err + ")"
	case v.path == "":
		note = "(no log file — this job's output is not on disk)"
	case len(rows) == 0 && j.state == "running":
		note = "(no output yet)"
	case len(rows) == 0:
		note = "(no output)"
	}
	v.maxOff = max(0, len(rows)-v.shown)
	if v.follow {
		v.off = v.maxOff
	}
	v.off = min(max(v.off, 0), v.maxOff)

	var ind []string
	if v.cut {
		ind = append(ind, "last "+commaf("%d bytes", jobLogTail))
	}
	if v.off > 0 {
		ind = append(ind, fmt.Sprintf("↑ %d more", v.off))
	}
	if below := len(rows) - v.off - v.shown; below > 0 {
		ind = append(ind, fmt.Sprintf("↓ %d more", below))
	}
	if v.follow && j.state == "running" {
		ind = append(ind, "following")
	}
	panelHead := gutter(!v.onCmd) + mark(true) + stTool.Render("output")
	if len(ind) > 0 {
		panelHead += "  " + stDim.Render(strings.Join(ind, " · "))
	}

	pos := "all"
	if len(rows) > v.shown {
		pos = fmt.Sprintf("%d–%d of %d", v.off+1, min(v.off+v.shown, len(rows)), len(rows))
	}
	t, _ := fitPlain(title, max(1, v.w-lipgloss.Width(pos)-2))
	out := make([]string, 0, v.h)
	out = append(out, row.style.Bold(true).Render(t)+stDim.Render("  "+pos))
	out = append(out, cmdRows...)
	out = append(out, fitRow(panelHead, v.w))
	if note != "" {
		out = append(out, "   "+stDim.Render(note))
	}
	cur := -1
	if len(v.matches) > 0 {
		cur = v.matches[v.at]
	}
	hit := map[int]bool{}
	for _, r := range v.matches {
		hit[r] = true
	}
	for i := v.off; i < min(len(rows), v.off+v.shown); i++ {
		r := rows[i]
		switch {
		case i == cur:
			r = stInspHit.Render(r)
		case hit[i]:
			r = stInspMatch.Render(r)
		}
		out = append(out, "   "+r)
	}
	body := v.h
	if footer != "" {
		body--
	}
	for len(out) < body {
		out = append(out, "")
	}
	out = out[:body]
	if footer != "" {
		out = append(out, footer)
	}
	v.lines = out
}

// view is the pane: exactly h rows of w.
func (v jobView) view(w, h int) string {
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(v.lines, "\n"))
}

// hint is the status line while the view is open.
func (v jobView) hint() string {
	if v.onCmd {
		return "job  ·  enter/→/← open/shut the command · ↓ output · / ? search · esc back"
	}
	return "job  ·  ↑/↓ pgup/pgdn g/G scroll · / ? search · n/N next · ← or esc back"
}

func (v *jobView) scroll(n int) {
	v.off = min(max(v.off+n, 0), v.maxOff)
	v.follow = v.off >= v.maxOff
}

// findMatches finds the rows containing the query, keeping the cursor.
func (v *jobView) findMatches() {
	v.matches = v.matches[:0]
	q := strings.ToLower(v.query)
	for i, r := range v.rows {
		if strings.Contains(strings.ToLower(r), q) {
			v.matches = append(v.matches, i)
		}
	}
	v.at = min(max(v.at, 0), max(0, len(v.matches)-1))
}

// jumpTo shows match at, wrapping at either end, near the middle of the panel.
func (v *jobView) jumpTo(at int) {
	if len(v.matches) == 0 {
		return
	}
	if at < 0 {
		at = len(v.matches) - 1
	} else if at >= len(v.matches) {
		at = 0
	}
	v.at, v.onCmd = at, false
	v.off = min(max(v.matches[at]-v.shown/2, 0), v.maxOff)
	v.follow = false
}

// firstMatch is where a new search starts: forward from the top of the
// panel, or backward from its bottom — less's rule, so a search of a tail
// finds what is on screen or nearest to it.
func (v *jobView) firstMatch() int {
	if v.dir > 0 {
		for i, r := range v.matches {
			if r >= v.off {
				return i
			}
		}
		return 0
	}
	for i := len(v.matches) - 1; i >= 0; i-- {
		if v.matches[i] < v.off+v.shown {
			return i
		}
	}
	return len(v.matches) - 1
}

// key handles one key and reports whether the view stays open.
func (v *jobView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.searching {
		switch msg.String() {
		case "esc":
			v.searching = false
			v.search.Blur()
		case "enter":
			v.query = strings.TrimSpace(v.search.Value())
			v.searching = false
			v.search.Blur()
			v.matches, v.at = nil, 0
			if v.query != "" {
				v.findMatches()
				v.jumpTo(v.firstMatch())
			}
		default:
			var cmd tea.Cmd
			v.search, cmd = v.search.Update(msg)
			return true, cmd
		}
		return true, nil
	}
	page := max(1, v.shown-1)
	switch msg.String() {
	case "q":
		return false, nil
	case "esc":
		if v.query != "" { // leave the search first, as in the conversation
			v.query, v.matches, v.at = "", nil, 0
			return true, nil
		}
		return false, nil
	case "left", "h":
		if v.onCmd && v.cmdOpen {
			v.cmdOpen = false
			return true, nil
		}
		return false, nil
	case "right", "l":
		if v.onCmd {
			v.cmdOpen = true
		}
	case "enter", " ":
		if v.onCmd {
			v.cmdOpen = !v.cmdOpen
		}
	case "up", "k":
		switch {
		case v.onCmd:
		case v.off > 0:
			v.scroll(-1)
		default: // the top of the output: the command is above it
			v.onCmd = true
		}
	case "down", "j":
		if v.onCmd {
			v.onCmd = false
		} else {
			v.scroll(1)
		}
	case "pgup":
		v.onCmd = false
		v.scroll(-page)
	case "pgdown":
		v.onCmd = false
		v.scroll(page)
	case "g", "home":
		v.onCmd = false
		v.scroll(-v.maxOff - 1)
	case "G", "end":
		v.onCmd = false
		v.scroll(v.maxOff + 1)
	case "/", "?":
		v.dir = 1
		if msg.String() == "?" {
			v.dir = -1
		}
		v.searching = true
		v.search.Prompt = msg.String()
		v.search.Reset()
		v.search.Focus()
		return true, textinput.Blink
	case "n":
		v.jumpTo(v.at + v.dir)
	case "N":
		v.jumpTo(v.at - v.dir)
	}
	return true, nil
}

// mouse scrolls the output on the wheel.
func (v *jobView) mouse(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll(-wheelDelta)
	case tea.MouseButtonWheelDown:
		v.scroll(wheelDelta)
	}
}

// ── the model's side ───────────────────────────────────────────────

// jobTailMsg is the view's read timer. gen ties it to one opening: a view
// closed and opened again starts a new timer, and the old one stops.
type jobTailMsg struct{ gen int }

func jobTailTick(gen int) tea.Cmd {
	return tea.Tick(jobTailEvery, func(time.Time) tea.Msg { return jobTailMsg{gen: gen} })
}

// openJobView opens job id in the conversation pane, reading its log now.
func (m *tuiModel) openJobView(id int) tea.Cmd {
	j := m.jobByID(id)
	if j == nil {
		return nil
	}
	m.jv = newJobView(id)
	m.jv.load(j.log)
	m.jobViewing = true
	m.jvGen++
	m.syncJobView()
	return jobTailTick(m.jvGen)
}

// syncJobView lays the view out for the job's current row at the pane's size.
// A job that is gone from the list closes it.
func (m *tuiModel) syncJobView() {
	if !m.jobViewing {
		return
	}
	j := m.jobByID(m.jv.id)
	if j == nil {
		m.jobViewing = false
		return
	}
	m.jv.layout(*j, m.vp.Width, m.convoHeight(), time.Now())
}

// jobTail reads the log for new output, on the timer.
func (m *tuiModel) jobTail(msg jobTailMsg) tea.Cmd {
	if !m.jobViewing || msg.gen != m.jvGen {
		return nil
	}
	if j := m.jobByID(m.jv.id); j != nil {
		m.jv.load(j.log)
	}
	m.syncJobView()
	return jobTailTick(msg.gen)
}
