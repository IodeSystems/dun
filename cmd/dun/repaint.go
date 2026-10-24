package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// /repaint: capture what dun believes is on screen and what the terminal
// actually shows, then draw everything again from dun's own state.
//
// USER 2026-10-23: "I have seen some situations where the screen is just
// mangled." The causes seen so far are outside the renderer's cell diff:
// bytes written to the tty by something else, and a resize that never reached
// Update. A full clear is the only repair that does not depend on knowing
// which one it was. The capture is taken FIRST, so the mangled screen is
// what gets recorded.
//
// What is reset:
//   - the terminal's modes, written straight to the tty: alt screen, SGR,
//     the G0 charset, scroll margins, origin and insert mode, autowrap — the
//     state a stray printf leaves behind and Bubble Tea never re-asserts;
//   - the renderer's screen cache (ClearScreen), so every cell is written again;
//   - the terminal size, asked for again (a missed SIGWINCH);
//   - holdConvo's padding, a mouse highlight, the wrap and viewport caches;
//   - mouse mode (as /mouse last set it), bracketed paste, a hidden cursor —
//     written with the reset, because the renderer sends a mode only when
//     View changes it, and it believes they are already on.
//
// Not reset: a block's grow-only height (convoEntry.maxH). It is the no-shrink
// rule rather than drawing state, and dropping it would move the conversation.
// Not re-pushed: the kitty keyboard flags — they are a stack, and a second
// push would need a second pop at exit.

// termReset puts the terminal modes a stray write can change back to what dun
// runs with. 1049h first: if something left the alt screen, this is the way
// back, and the ClearScreen that follows paints the alt screen whole.
const termReset = "\x1b[?1049h" + // alt screen
	"\x1b[0m" + // SGR
	"\x1b(B" + // G0 = ASCII (a stray ESC ( 0 draws letters as line art)
	"\x1b[r" + // scroll margins = the whole screen (cursor goes home)
	"\x1b[?6l" + // origin mode off
	"\x1b[4l" + // insert mode off
	"\x1b[?7h" // autowrap on

// repaintDoneMsg arrives after the clears.
type repaintDoneMsg struct {
	dir    string // the capture ("" when it failed)
	err    error  // why it failed
	report bool   // /repaint report: go on to the model (repaintreport.go)
}

// repaintSlash is /repaint [report].
func repaintSlash(m *tuiModel, args []string) tea.Cmd {
	report := len(args) > 0 && args[0] == "report"
	if len(args) > 0 && !report {
		m.append(stErr.Render("usage: /repaint [report]"))
		return nil
	}
	return tea.Tick(repaintSettle, func(time.Time) tea.Msg { return repaintNowMsg{report: report} })
}

// repaintSettle is how long the capture waits: the renderer flushes at most
// every 1/60 s, so the frame that closed the palette (and anything else in the
// same burst of keys) is on the terminal by then. Captured at once, the
// expected screen ran a frame ahead of the terminal — seen live, five rows
// differed that nothing had mangled.
const repaintSettle = 50 * time.Millisecond

// repaintNowMsg is the settled moment: capture, then repaint.
type repaintNowMsg struct{ report bool }

func (m *tuiModel) repaintNow(msg repaintNowMsg) tea.Cmd {
	dir, err := m.capture(time.Now())
	return m.repaint(repaintDoneMsg{dir: dir, err: err, report: msg.report})
}

// repaint drops every cache and hold between dun's state and the screen and
// returns the commands that clear and redraw it, in order: the reset reaches
// the terminal before the full redraw the clear asks for.
func (m *tuiModel) repaint(done repaintDoneMsg) tea.Cmd {
	m.dropHold()
	m.drag = mouseSel{}
	for i := range m.convo {
		m.convo[i].invalidateWrap()
	}
	if m.vpc != nil {
		m.vpc.valid = false
	}
	if m.w > 0 && m.h > 0 {
		m.refresh()
	}
	return tea.Sequence(
		tea.Raw(termReset+modeReset(m.mouseMode)),
		tea.ClearScreen,
		tea.RequestWindowSize,
		func() tea.Msg { return done },
	)
}

// modeReset re-asserts the modes View declares: mouse as /mouse set it,
// bracketed paste, a hidden cursor.
func modeReset(mouse string) string {
	s := ansi.SetModeBracketedPaste + ansi.HideCursor
	switch mouseModeOf(mouse) {
	case tea.MouseModeAllMotion:
		s += ansi.SetModeMouseAnyEvent + ansi.SetModeMouseExtSgr
	case tea.MouseModeCellMotion:
		s += ansi.SetModeMouseButtonEvent + ansi.SetModeMouseExtSgr
	}
	return s
}

// repaintDone runs once the clears are through.
func (m *tuiModel) repaintDone(msg repaintDoneMsg) {
	if msg.err != nil {
		m.append(stDim.Render("repainted · capture failed: " + msg.err.Error()))
		return
	}
	m.append(stDim.Render("repainted · capture saved to " + tildePath(msg.dir) + "/"))
	if msg.report {
		m.startReport(msg.dir)
	}
}

// tildePath shows a path under $HOME as ~/….
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// ── the frame dun believes is on screen ────────────────────────────

// shownFrame is the last string View handed the renderer. Behind a pointer
// for the reason vpc is: View has a value receiver.
type shownFrame struct{ s string }

// expectedRows rebuilds the screen dun intends, one string per terminal row.
//
// It is NOT the current View. The renderer draws the last View, but only
// after keeping its last h lines and cutting each to the width. So: the last
// frame, as the renderer cut it.
func (m tuiModel) expectedRows() []string {
	s := ""
	if m.shown != nil {
		s = m.shown.s
	}
	rows := strings.Split(s, "\n")
	if m.h > 0 && len(rows) > m.h {
		rows = rows[len(rows)-m.h:]
	}
	if m.w > 0 {
		for i, r := range rows {
			rows[i] = ansi.Truncate(r, m.w, "")
		}
	}
	for len(rows) < m.h {
		rows = append(rows, "")
	}
	return rows
}

// ── the capture ────────────────────────────────────────────────────

// runTmux runs tmux; a variable so a test can stand in for it.
var runTmux = func(args ...string) ([]byte, error) {
	return exec.Command("tmux", args...).Output()
}

// ttySize is the size the tty reports now; a variable for the same reason.
var ttySize = func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }

// capture writes $DUN_HOME/bugreports/<ts>/ and returns its path.
func (m tuiModel) capture(now time.Time) (string, error) {
	root := filepath.Join(dunHome(), "bugreports")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir := filepath.Join(root, now.Format("20060102-150405"))
	for n := 2; ; n++ {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !os.IsExist(err) || n > 20 {
			return "", err
		}
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", now.Format("20060102-150405"), n))
	}
	exp := m.expectedRows()
	expTxt := make([]string, len(exp))
	for i, r := range exp {
		expTxt[i] = plainRow(r)
	}
	files := map[string]string{
		"expected.ansi": strings.Join(exp, "\n") + "\n",
		"expected.txt":  strings.Join(expTxt, "\n") + "\n",
		"events.jsonl":  m.evlog.jsonl(),
	}
	st := m.captureState(now)
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" {
		files["actual.txt"] = "not available: not in tmux\n"
		st.Actual = "not available: not in tmux"
	} else if a, err := runTmux("capture-pane", "-p", "-e", "-t", pane); err != nil {
		files["actual.txt"] = "not available: tmux capture-pane: " + err.Error() + "\n"
		st.Actual = "not available: " + err.Error()
	} else {
		act := strings.Split(strings.TrimSuffix(string(a), "\n"), "\n")
		actTxt := make([]string, len(act))
		for i, r := range act {
			actTxt[i] = plainRow(r)
		}
		files["actual.ansi"] = strings.Join(act, "\n") + "\n"
		files["actual.txt"] = strings.Join(actTxt, "\n") + "\n"
		files["diff.txt"] = diffRows(expTxt, actTxt)
		st.Actual = "tmux capture-pane " + pane
		if sz, err := runTmux("display", "-p", "-t", pane, "#{pane_width}x#{pane_height}"); err == nil {
			st.Size.Tmux = strings.TrimSpace(string(sz))
		}
	}
	js, _ := json.MarshalIndent(st, "", "  ")
	files["state.json"] = string(js) + "\n"
	files["README.txt"] = captureReadme
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

const captureReadme = `dun /repaint capture

expected.txt   the screen dun believes it drew: its last frame, cut to the
expected.ansi  terminal size (.txt without escape codes, .ansi with them)
actual.txt     what the terminal shows, from tmux capture-pane (or why it is
actual.ansi    not available: outside tmux there is no way to read it back)
diff.txt       the rows where expected and actual differ, both versions
state.json     sizes (dun, the tty, tmux), terminal env, mouse mode, layout
               numbers, focus, open pickers, holds, the
               dun version and the session id (taken as /repaint runs, so
               the composer has already been cleared)
events.jsonl   the last 200 messages the TUI handled, oldest first: time,
               kind and a short summary (at most 80 characters of text)

/repaint report adds:
prered/        these files after the deterministic pass ($HOME, user and
               host names, e-mail addresses, token-shaped strings)
prompt.txt     exactly what was sent to the model
model-output.txt  what it answered
redacted/      the model's redacted copy, each row at its width; the .txt
               screens and diff.txt are made from its .ansi screens
explanation.md what the model thinks went wrong

Nothing here is sent anywhere unless you send it.
`

// plainRow is a screen row as text: no escape codes, and no other control
// bytes. tmux marks line-drawing cells with SO/SI (ESC ( 0 written to the
// pane does it), which ansi.Strip keeps.
func plainRow(s string) string {
	s = ansi.Strip(s)
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }

// diffRows compares two screens row by row (trailing blanks ignored) and
// lists the rows that differ, both versions.
func diffRows(exp, act []string) string {
	var b strings.Builder
	n := max(len(exp), len(act))
	var rows []int
	for i := 0; i < n; i++ {
		if rowAt(exp, i) != rowAt(act, i) {
			rows = append(rows, i)
		}
	}
	fmt.Fprintf(&b, "expected (dun) vs actual (terminal): %d and %d rows, %d differ\n", len(exp), len(act), len(rows))
	for _, i := range rows {
		fmt.Fprintf(&b, "\nrow %d\n  expected: %s\n  actual:   %s\n", i+1, rowAt(exp, i), rowAt(act, i))
	}
	return b.String()
}

func rowAt(rows []string, i int) string {
	if i < len(rows) {
		return strings.TrimRight(rows[i], " ")
	}
	return ""
}

// captureState is state.json.
type captureState struct {
	Time    string            `json:"time"`
	Version string            `json:"version"`
	Commit  string            `json:"commit,omitempty"`
	Session string            `json:"session"`
	Size    captureSizes      `json:"size"`
	Env     map[string]string `json:"env"`
	Mouse   string            `json:"mouse"`
	Layout  captureLayout     `json:"layout"`
	Actual  string            `json:"actual"`
}

type captureSizes struct {
	Dun  string `json:"dun"`            // what the last WindowSizeMsg said
	TTY  string `json:"tty"`            // what the tty says now
	Tmux string `json:"tmux,omitempty"` // tmux display #{pane_width}x#{pane_height}
}

type captureLayout struct {
	Head        int    `json:"head_height"`
	Activity    int    `json:"activity_height"`
	Convo       int    `json:"convo_height"`
	ConvoBudget int    `json:"convo_height_budget"`
	ConvoLines  int    `json:"convo_lines"`
	YOffset     int    `json:"convo_yoffset"`
	Pinned      bool   `json:"pinned"`
	Input       int    `json:"input_height"`
	Upgrade     int    `json:"upgrade_height"`
	Band        int    `json:"band_rows"`
	Focus       string `json:"focus"`
	Open        string `json:"open"` // the question, picker or overlay that has the keys
	Hold        int    `json:"convo_hold"`
	HoldW       int    `json:"convo_hold_width"`
	Busy        bool   `json:"busy"`
	Entries     int    `json:"convo_entries"`
}

func (m tuiModel) captureState(now time.Time) captureState {
	st := captureState{
		Time:    now.Format(time.RFC3339),
		Version: version,
		Session: m.sessionID,
		Env:     map[string]string{},
		Mouse:   m.mouseMode,
	}
	if st.Session == "" && m.proc != nil {
		st.Session = m.proc.session
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				st.Commit = s.Value
			}
		}
	}
	st.Size.Dun = fmt.Sprintf("%dx%d", m.w, m.h)
	if w, h, err := ttySize(); err == nil {
		st.Size.TTY = fmt.Sprintf("%dx%d", w, h)
	} else {
		st.Size.TTY = "unknown: " + err.Error()
	}
	for _, k := range []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TMUX", "TMUX_PANE"} {
		st.Env[k] = os.Getenv(k)
	}
	l := &st.Layout
	l.Head = lipgloss.Height(m.headView())
	if a := m.activityView(); a != "" {
		l.Activity = lipgloss.Height(a)
	}
	l.Convo, l.ConvoBudget = m.vp.Height, m.convoHeightBudget()
	l.ConvoLines, l.YOffset, l.Pinned = len(m.vp.lines), m.vp.YOffset, m.scrollPinned
	l.Input = lipgloss.Height(m.lowerView())
	if u := m.upgradeView(); u != "" {
		l.Upgrade = lipgloss.Height(u)
	}
	if m.bandShown() {
		l.Band = len(m.bandRaw)
	}
	l.Focus = map[int]string{focusInput: "input", focusConvo: "convo", focusActivity: "activity"}[m.focus]
	l.Open = m.openName()
	l.Hold, l.HoldW = m.convoHold, m.convoHoldW
	l.Busy, l.Entries = m.busy, len(m.convo)
	return st
}

// openName names what has the keys besides the composer.
func (m tuiModel) openName() string {
	switch {
	case m.inspecting:
		return "inspector"
	case m.paging:
		return "pager"
	case m.jobViewing:
		return "job view"
	case m.confirm.q != "":
		return "confirm"
	case m.asking:
		return fmt.Sprintf("question (%d options, multi=%v)", len(m.askOptions), m.askMulti)
	case m.askParked:
		return "question parked"
	case m.picking:
		return "session picker"
	case m.modelPicking:
		return "model picker"
	case m.mcpForm:
		return "mcp form"
	case m.searching || m.searchActive:
		return "search"
	case m.paletteActive():
		return "palette"
	}
	return ""
}

// ── the event ring ─────────────────────────────────────────────────

// evlogSize is how many messages the ring keeps.
const evlogSize = 200

// eventRing is the last evlogSize messages Update saw. A fixed array written
// in place: recording one costs the summary string and nothing else.
//
// A run of the same message with nothing to say (spinner and cursor ticks,
// ten a second) is one record with a count: measured live, the ticks alone
// filled all 200 entries in about ten seconds.
type eventRing struct {
	buf  [evlogSize]evRec
	next int // where the next one goes
	n    int // how many are held (≤ evlogSize)
}

type evRec struct {
	at   time.Time
	last time.Time // the last of a run (n > 1)
	n    int       // how many in the run
	kind string
	ev   bool // kind is an engine event's type: written "ev:<type>"
	info string
}

func newEventRing() *eventRing { return &eventRing{} }

// add records one message. kind is msgKind's (computed once per Update).
//
// Engine events arrive wrapped in a procMsg (which engine sent it), so the
// wrapper is looked through: seen live, every engine event was recorded as
// "main.procMsg" with nothing said about it. The event's type is kept as it
// is, without the "ev:" prefix, so recording it allocates nothing.
func (r *eventRing) add(at time.Time, kind string, msg tea.Msg) {
	if r == nil {
		return
	}
	if pm, ok := msg.(procMsg); ok {
		msg = pm.msg
		if _, isEv := msg.(evMsg); !isEv {
			kind = msgKind(msg) // eofMsg and the like: rare
		}
	}
	ev := false
	if e, ok := msg.(evMsg); ok {
		if t, ok := e["type"].(string); ok {
			kind, ev = t, true
		}
	}
	info := msgSummary(msg)
	if prev := &r.buf[(r.next-1+evlogSize)%evlogSize]; r.n > 0 && info == "" && prev.info == "" && prev.kind == kind && prev.ev == ev {
		prev.last, prev.n = at, prev.n+1
		return
	}
	r.buf[r.next] = evRec{at: at, n: 1, kind: kind, ev: ev, info: info}
	r.next = (r.next + 1) % evlogSize
	if r.n < evlogSize {
		r.n++
	}
}

// records returns the held records, oldest first.
func (r *eventRing) records() []evRec {
	if r == nil {
		return nil
	}
	out := make([]evRec, 0, r.n)
	start := (r.next - r.n + evlogSize) % evlogSize
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%evlogSize])
	}
	return out
}

func (r *eventRing) jsonl() string {
	var b strings.Builder
	for _, e := range r.records() {
		rec := struct {
			T     string `json:"t"`
			Kind  string `json:"kind"`
			Info  string `json:"info,omitempty"`
			N     int    `json:"n,omitempty"`     // a run of n
			Until string `json:"until,omitempty"` // the run's last
		}{T: e.at.Format("15:04:05.000"), Kind: e.kind, Info: e.info}
		if e.ev {
			rec.Kind = "ev:" + e.kind
		}
		if e.n > 1 {
			rec.N, rec.Until = e.n, e.last.Format("15:04:05.000")
		}
		line, _ := json.Marshal(rec)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// evTextMax is the most conversation text a summary carries.
const evTextMax = 80

// msgSummary is a short description of a message: its size or its subject,
// and at most evTextMax characters of anything anyone wrote. Typed keys are
// named, not spelled: a run of letters is someone's message.
func msgSummary(msg tea.Msg) string {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return strconv.Itoa(msg.Width) + "x" + strconv.Itoa(msg.Height)
	case tea.KeyPressMsg:
		if typedText(msg) {
			return "rune"
		}
		return msg.String()
	case tea.PasteMsg:
		return "paste " + strconv.Itoa(utf8.RuneCountInString(msg.Content)) + " runes"
	case tea.MouseMsg:
		mo := msg.Mouse()
		return msg.String() + " " + strconv.Itoa(mo.X) + "," + strconv.Itoa(mo.Y)
	case evMsg:
		switch t, _ := msg["type"].(string); t {
		case "token", "reasoning":
			s, _ := msg["text"].(string)
			return strconv.Itoa(len(s)) + " bytes"
		case "tool_call", "tool_result":
			s, _ := msg["name"].(string)
			return s
		default:
			for _, k := range []string{"text", "content", "message", "question"} {
				if s, ok := msg[k].(string); ok && s != "" {
					return clipRunes(oneLine(s), evTextMax)
				}
			}
		}
	}
	return ""
}

// clipRunes cuts s to n runes, never inside one.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
