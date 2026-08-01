package main

// ── the activity zone ───────────────────────────────────────────────
//
// The top of the screen is two lines: what you last asked for, and what is
// running because of it.
//
// Both halves exist for the same reason. Sub-agents are spawned by the MODEL,
// stay resident until dismissed, and have no concurrency cap; background jobs
// outlive the turn that started them and are silent by design. agent_monitor
// and exec_monitor are the MODEL's view of those two. This is the human's, and
// without it a forgotten child or a job that died in its second second is
// invisible rather than merely idle.
//
// It is a TREE, and the rule is uniform with the rest of the UI: `▸` means
// descendable, → opens and descends, ← ascends. Collapsed it costs one line no
// matter how many children exist; descended it is a table you can select in;
// descending again opens a job's output or switches scope to an agent.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Descent levels within the zone. Focus alone does not expand it — → does,
// which is what makes the affordance mean the same thing here as everywhere
// else on screen.
const (
	actCollapsed = iota
	actList
)

// actMaxRows caps the expanded table and scrolls within it. Children are
// uncapped and jobs accumulate for the life of the session, so an unbounded
// table would eventually be the whole screen.
const actMaxRows = 6

// actKey identifies a row across rebuilds. The row slice is rebuilt on every
// event, and a new AGENT shifts every JOB index down — so a selection stored as
// an index would silently walk to a different row while you were reading it.
type actKey struct {
	agent bool
	id    int
}

// actRow is one thing the session is doing, agent or job flattened to the same
// shape. Both the collapsed line and the table render from this, so they can
// never disagree about what is happening.
type actRow struct {
	key    actKey
	mark   string
	style  lipgloss.Style
	label  string // "#1 running 2m 6.5kt" — identity and cost
	detail string // what it is doing
	// attn marks a row that will not resolve itself: a child blocked on
	// ask_parent, or a job that failed. It survives clipping (see actLine).
	attn bool
}

// agentRow is one child as the TUI shows it.
type agentRow struct {
	id      int
	state   string
	status  string
	prompt  string
	model   string
	tokens  int
	seconds int
	blocked bool
}

// jobRow is one background job as the TUI shows it. Times are UNIX SECONDS
// rather than a pre-computed elapsed: the engine pushes a job on start and on
// finish and not in between, so a duration measured at push time would sit
// frozen on screen for the whole run. The UI ticks its own clock against these.
type jobRow struct {
	id      int
	command string
	log     string
	state   string // running · ok · failed · timeout · error
	bytes   int64
	started int64
	ended   int64
	code    int
	muted   bool
}

func agentRowsFromEvent(ev evMsg) []agentRow {
	raw, _ := ev["agents"].([]any)
	out := make([]agentRow, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, agentRow{
			id: num(m["id"]), state: str(m["state"]), status: str(m["status"]),
			prompt: str(m["prompt"]), model: str(m["model"]),
			tokens: num(m["tokens"]), seconds: num(m["seconds"]),
			blocked: m["blocked"] == true,
		})
	}
	return out
}

func jobRowsFromEvent(ev evMsg) []jobRow {
	raw, _ := ev["jobs"].([]any)
	out := make([]jobRow, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, jobRow{
			id: num(m["id"]), command: str(m["command"]), log: str(m["log"]),
			state: str(m["state"]), bytes: num64(m["bytes"]),
			started: num64(m["started"]), ended: num64(m["ended"]),
			code: num(m["code"]), muted: m["muted"] == true,
		})
	}
	return out
}

// elapsed is how long the job has been going, measured to now while it runs.
func (j jobRow) elapsed(now time.Time) int {
	end := j.ended
	if end == 0 {
		end = now.Unix()
	}
	if j.started == 0 || end < j.started {
		return 0
	}
	return int(end - j.started)
}

func (j jobRow) running() bool { return j.state == "running" }

// liveAgents counts the ones still costing something — a dismissed child is
// history, and a header that counted it would keep growing forever.
func (m tuiModel) liveAgents() int {
	n := 0
	for _, a := range m.agents {
		if a.state == "running" || a.state == "idle" {
			n++
		}
	}
	return n
}

// actTicking reports whether anything in the zone has a clock that needs
// ticking. Only jobs do — an agent's elapsed is pushed per chat round.
func (m tuiModel) actTicking() bool {
	for _, j := range m.jobs {
		if j.running() {
			return true
		}
	}
	return false
}

// actRows flattens agents and jobs into one selectable list.
//
// Agents first, then jobs, each in id order — a STABLE order, deliberately not
// sorted by interest. Rows that reorder themselves as states change would move
// under a selection that is already on one.
func (m tuiModel) actRows() []actRow {
	if m.scopeAgent != 0 {
		return m.scopeRows()
	}
	now := time.Now()
	rows := make([]actRow, 0, len(m.agents)+len(m.jobs))
	for _, a := range m.agents {
		rows = append(rows, agentActRow(a))
	}
	for _, j := range m.jobs {
		rows = append(rows, jobActRow(j, now))
	}
	return rows
}

func agentActRow(a agentRow) actRow {
	// A blocked child is the one state that never resolves itself, so it gets
	// the loud colour and says what it is waiting for.
	mark, style := "·", stDim
	switch {
	case a.blocked:
		mark, style = "?", stAsk
	case a.state == "running":
		mark, style = "▸", stTool
	case a.state == "idle":
		mark, style = "✓", stNote
	case a.state == "stopped":
		mark = "⏸"
	}
	label := fmt.Sprintf("#%d %s", a.id, a.state)
	if a.seconds > 0 {
		label += " " + compactDur(a.seconds)
	}
	if a.tokens > 0 {
		label += " " + compactTokens(a.tokens)
	}
	// What it is DOING beats what it was asked, once it has said anything.
	detail := a.status
	if detail == "" {
		detail = a.prompt
	}
	if a.blocked {
		detail = "waiting on you — answer it"
	}
	return actRow{
		key: actKey{agent: true, id: a.id}, mark: mark, style: style,
		label: label, detail: detail, attn: a.blocked,
	}
}

func jobActRow(j jobRow, now time.Time) actRow {
	mark, style, state := "⚙", stTool, j.state
	switch j.state {
	case "running":
	case "ok":
		mark, style = "✓", stDim
	case "failed":
		mark, style, state = "✗", stErr, fmt.Sprintf("failed (exit %d)", j.code)
	case "timeout", "error":
		mark, style = "✗", stErr
	default:
		mark, style = "·", stDim
	}
	label := fmt.Sprintf("job %d %s", j.id, state)
	if s := j.elapsed(now); s > 0 {
		label += " " + compactDur(s)
	}
	if j.muted {
		label += " muted"
	}
	return actRow{
		key: actKey{id: j.id}, mark: mark, style: style,
		label: label, detail: j.command,
		attn: j.state == "failed" || j.state == "timeout" || j.state == "error",
	}
}

// taskView is the top line: the last thing the human asked for. It is what
// every row below it is ultimately in service of, and in a long session the
// message that started the work has usually scrolled away.
func (m tuiModel) taskView() string {
	if m.task == "" || m.w < 20 {
		return ""
	}
	text, _ := fitPlain(oneLine(m.task), m.w-3)
	return stUser.Render("› ") + stDim.Render(text)
}

// activityView is the zone itself: one line collapsed, a scrolling table
// descended. Empty when there is nothing to show, so a session that never
// delegates and never backgrounds a command loses no space to it.
func (m tuiModel) activityView() string {
	rows := m.actRows()
	if len(rows) == 0 || m.w < 20 {
		return ""
	}
	if m.actLevel == actCollapsed {
		return m.actLine(rows)
	}
	return m.actTable(rows)
}

// actLine is the collapsed one-liner: every row's mark and identity, then the
// detail of whichever row most needs reading, clipped to the width.
//
// The `▸` prefix is the affordance, and it means here exactly what it means on
// a docs block in the conversation.
func (m tuiModel) actLine(rows []actRow) string {
	lead := leadRow(rows)
	attn := lead != nil && lead.attn
	glyph, gstyle := "▸ ", stDim
	if attn {
		// A row that needs answering must survive clipping — the detail that
		// says so is the first thing to be cut off on a narrow terminal.
		gstyle = stAsk
	}
	var b strings.Builder
	b.WriteString(gstyle.Render(glyph))
	used := lipgloss.Width(glyph)
	budget := m.w - 1

	for i, r := range rows {
		seg := r.mark + r.label
		if i > 0 {
			seg = " · " + seg
		}
		text, cut := fitPlain(seg, budget-used)
		if text == "" {
			return b.String()
		}
		b.WriteString(r.style.Render(text))
		used += lipgloss.Width(text)
		if cut {
			return b.String()
		}
	}
	if lead != nil && lead.detail != "" {
		text, _ := fitPlain("  ⟩ "+lead.detail, budget-used)
		b.WriteString(stDim.Render(text))
	}
	return b.String()
}

// leadRow is the one row the collapsed line describes in words: attention
// first, then anything running, else nothing.
func leadRow(rows []actRow) *actRow {
	for i := range rows {
		if rows[i].attn {
			return &rows[i]
		}
	}
	for i := range rows {
		if rows[i].mark == "▸" || rows[i].mark == "⚙" {
			return &rows[i]
		}
	}
	return nil
}

// actTable is the descended view: one row per item, a cursor on the selected
// one, scrolled to keep the selection inside actMaxRows.
func (m tuiModel) actTable(rows []actRow) string {
	sel := m.actIndex(rows)
	off := actWindow(sel, len(rows))
	end := off + actMaxRows
	if end > len(rows) {
		end = len(rows)
	}
	out := make([]string, 0, actMaxRows+1)
	head := "▾ " + actCounts(rows)
	if len(rows) > actMaxRows {
		head += fmt.Sprintf("  (%d–%d of %d)", off+1, end, len(rows))
	}
	out = append(out, stDim.Render(head))
	for i := off; i < end; i++ {
		r := rows[i]
		cursor, budget := "  ", m.w-1
		if i == sel {
			cursor = stSel.Render("➤ ")
		}
		body, cut := fitPlain(r.mark+" "+r.label, budget-2)
		line := cursor + r.style.Render(body)
		if !cut && r.detail != "" {
			d, _ := fitPlain("  "+r.detail, budget-2-lipgloss.Width(body))
			line += stDim.Render(d)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// actWindow keeps the selection inside the visible slice.
func actWindow(sel, n int) int {
	if n <= actMaxRows || sel < 0 {
		return 0
	}
	off := sel - actMaxRows/2
	if off < 0 {
		off = 0
	}
	if off > n-actMaxRows {
		off = n - actMaxRows
	}
	return off
}

func actCounts(rows []actRow) string {
	agents, jobs := 0, 0
	for _, r := range rows {
		if r.key.agent {
			agents++
		} else {
			jobs++
		}
	}
	parts := make([]string, 0, 2)
	if agents > 0 {
		parts = append(parts, plural(agents, "agent"))
	}
	if jobs > 0 {
		parts = append(parts, plural(jobs, "job"))
	}
	return strings.Join(parts, " · ")
}

// actIndex resolves the stored selection key to an index in the current rows,
// falling back to the first row when what was selected has gone.
func (m tuiModel) actIndex(rows []actRow) int {
	for i, r := range rows {
		if r.key == m.actSel {
			return i
		}
	}
	if len(rows) == 0 {
		return -1
	}
	return 0
}

// actMove steps the selection by d rows.
func (m *tuiModel) actMove(d int) {
	rows := m.actRows()
	if len(rows) == 0 {
		return
	}
	i := m.actIndex(rows) + d
	if i < 0 {
		i = 0
	}
	if i >= len(rows) {
		i = len(rows) - 1
	}
	m.actSel = rows[i].key
}

// actSelected is the row the cursor is on, if any.
func (m tuiModel) actSelected() *actRow {
	rows := m.actRows()
	i := m.actIndex(rows)
	if i < 0 {
		return nil
	}
	return &rows[i]
}

// jobByID finds a job row for the detail view.
func (m tuiModel) jobByID(id int) *jobRow {
	for i := range m.jobs {
		if m.jobs[i].id == id {
			return &m.jobs[i]
		}
	}
	return nil
}

// agentByID finds an agent row for the detail view.
func (m tuiModel) agentByID(id int) *agentRow {
	for i := range m.agents {
		if m.agents[i].id == id {
			return &m.agents[i]
		}
	}
	return nil
}

// fitPlain truncates a PLAIN (unstyled) string to at most n columns, adding an
// ellipsis when it cuts, and reports whether it did.
//
// Styling is applied AFTER, never before: clipping a string that already has
// escape codes in it counts those codes as characters and can leave a colour
// turned on for the rest of the line.
func fitPlain(s string, n int) (string, bool) {
	if n <= 0 {
		return "", true
	}
	if lipgloss.Width(s) <= n {
		return s, false
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if used+w > n-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…", true
}

func compactDur(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
}

func compactTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%dt", n)
	}
	return fmt.Sprintf("%.1fkt", float64(n)/1000)
}

// num decodes a JSON number, which arrives as float64 through encoding/json.
func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func num64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// ── focus ───────────────────────────────────────────────────────────

// cycleFocus moves d zones through input → convo → activity → input, in screen
// order from the bottom up. An empty activity zone is skipped rather than
// focused-but-blank, which is what preserves the old two-way tab for every
// session that never delegates.
func (m *tuiModel) cycleFocus(d int) {
	zones := []int{focusInput, focusConvo}
	if len(m.actRows()) > 0 {
		zones = append(zones, focusActivity)
	}
	at := 0
	for i, z := range zones {
		if z == m.focus {
			at = i
		}
	}
	m.setFocus(zones[((at+d)%len(zones)+len(zones))%len(zones)])
}

// setFocus applies the per-zone entry state: the input takes the cursor back,
// the conversation starts on the newest message, and the activity zone starts
// collapsed with a valid selection.
func (m *tuiModel) setFocus(z int) {
	m.focus = z
	switch z {
	case focusInput:
		m.input.Focus()
	case focusConvo:
		m.input.Blur()
		if m.sel < 0 || m.sel >= len(m.convo) {
			m.sel = len(m.convo) - 1
		}
	case focusActivity:
		m.input.Blur()
		if rows := m.actRows(); len(rows) > 0 && m.actIndex(rows) < 0 {
			m.actSel = rows[0].key
		}
	}
}

// actKeys drives the zone: ↑/↓ pick a row, → opens and descends, ← ascends and
// finally leaves. Returns false when the key is not ours.
//
// The `▸`/→/← rule is the whole point: it means the same thing here, on a docs
// block in the conversation, and on a job's output. One affordance, learned
// once.
func (m *tuiModel) actKeys(key string) (tea.Cmd, bool) {
	rows := m.actRows()
	if len(rows) == 0 {
		m.setFocus(focusInput)
		return textinput.Blink, true
	}
	switch key {
	case "up":
		if m.actLevel == actList {
			m.actMove(-1)
		}
		return nil, true
	case "down":
		if m.actLevel == actList {
			m.actMove(1)
		}
		return nil, true
	case "right", "enter":
		if m.actLevel == actCollapsed {
			m.actLevel = actList
			return nil, true
		}
		return m.actDescend(), true
	case "left", "esc":
		if m.actLevel == actList {
			m.actLevel = actCollapsed
			return nil, true
		}
		// Ascending out of the top of the zone goes home, to the input.
		m.setFocus(focusInput)
		return textinput.Blink, true
	}
	return nil, false
}

// actDescend opens the selected row: a job shows its command and its log, an
// agent switches scope to that child.
func (m *tuiModel) actDescend() tea.Cmd {
	row := m.actSelected()
	if row == nil {
		return nil
	}
	if row.key == parentKey {
		return m.leaveAgentScope()
	}
	if row.key.agent {
		return m.enterAgentScope(row.key.id)
	}
	j := m.jobByID(row.key.id)
	if j == nil {
		return nil
	}
	// The inspector is already scrollable and searchable, and a job's log is
	// exactly the kind of thing it exists for. No second widget.
	m.insp = newInspector("exec #"+fmt.Sprint(j.id), j.command, m.jobDetail(*j))
	m.insp.setSize(m.w, m.h)
	m.inspecting = true
	return nil
}

// jobDetail is what a job's inspector shows: the verdict, where the log is, and
// as much of it as the TUI can read. The engine wrote the log to a path it
// handed us, so the tail is read here rather than round-tripped.
func (m tuiModel) jobDetail(j jobRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "command: %s\n", j.command)
	fmt.Fprintf(&b, "state:   %s", j.state)
	if j.state == "failed" {
		fmt.Fprintf(&b, " (exit %d)", j.code)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "elapsed: %s\n", compactDur(j.elapsed(time.Now())))
	fmt.Fprintf(&b, "output:  %d bytes\n", j.bytes)
	if j.muted {
		b.WriteString("muted:   the model asked not to be told about this one\n")
	}
	if j.log == "" {
		b.WriteString("\n(no log file — this session is in memory)")
		return b.String()
	}
	fmt.Fprintf(&b, "log:     %s\n\n", j.log)
	data, err := os.ReadFile(j.log)
	if err != nil {
		b.WriteString("(log unreadable: " + err.Error() + ")")
		return b.String()
	}
	if len(data) > jobLogTail {
		b.WriteString("…(showing the last " + fmt.Sprint(jobLogTail) + " bytes)\n")
		data = data[len(data)-jobLogTail:]
	}
	b.Write(data)
	return b.String()
}

// jobLogTail bounds what the inspector loads. A build log can be tens of
// megabytes and the failure is always at the bottom.
const jobLogTail = 256 * 1024

// ── agent scope ─────────────────────────────────────────────────────
//
// Descending into an agent is a SCOPE SWITCH, not an overlay: the task line
// becomes the child's prompt, the conversation becomes the child's, and the
// activity strip becomes a `↰ parent` row that takes you back. Depth-1 in
// practice, since children cannot spawn children, but the model is recursive
// and costs nothing extra.
//
// The input stays live and routes to that child. This is a new human→child
// channel that children were deliberately denied (they have no ask_user), and
// the parent only learns of it when the child next reports. Accepted on
// purpose: the human is not the model, and the point of the pane is that a
// child is steerable rather than merely observable.

// parentKey is the `↰ parent` row — an agent row with no id, since agent ids
// start at 1.
var parentKey = actKey{agent: true, id: 0}

// enterAgentScope swaps the conversation for a child's and asks the engine for
// it. The child's scrollback arrives later as an agent_history event.
func (m *tuiModel) enterAgentScope(id int) tea.Cmd {
	a := m.agentByID(id)
	if a == nil {
		return nil
	}
	if m.scopeAgent == 0 {
		// Only the ROOT conversation is stashed; scope does not nest, so a
		// second descent must not overwrite the way home.
		m.rootConvo, m.rootTask = m.convo, m.task
	}
	m.scopeAgent = id
	m.convo = nil
	m.task = a.prompt
	m.actLevel = actCollapsed
	m.actSel = parentKey
	m.setFocus(focusInput)
	m.input.placeholder = fmt.Sprintf("tell agent #%d…", id)
	m.refresh()
	m.proc.agentCmd(id, "view", "")
	return textinput.Blink
}

// leaveAgentScope restores the root conversation.
func (m *tuiModel) leaveAgentScope() tea.Cmd {
	if m.scopeAgent == 0 {
		return nil
	}
	m.scopeAgent = 0
	m.convo, m.task = m.rootConvo, m.rootTask
	m.rootConvo, m.rootTask = nil, ""
	m.actLevel = actCollapsed
	m.input.placeholder = "ask dun to do something…"
	m.setFocus(focusInput)
	m.refresh()
	return textinput.Blink
}

// scopeRows is the activity strip inside agent scope: the way back, and
// nothing else. A child's own background jobs are not forwarded — its callbacks
// are nil by design — so claiming to list them would be a lie.
func (m tuiModel) scopeRows() []actRow {
	a := m.agentByID(m.scopeAgent)
	label := fmt.Sprintf("↰ parent (viewing agent #%d", m.scopeAgent)
	if a != nil {
		label += " · " + a.state
	}
	return []actRow{{
		key: parentKey, mark: "↰", style: stNote,
		label: label + ")", detail: "→ to go back",
	}}
}
