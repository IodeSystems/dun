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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// /repaint: capture what dun believes is on screen and what the terminal
// actually shows, then draw everything again from dun's own state.
//
// USER 2026-10-23: "I have seen some situations where the screen is just
// mangled." The causes seen so far are outside dun's line diff: bytes written
// to the tty by something else, a resize that never reached Update, and the
// scroll region (scrollregion.go), which draws rows the renderer never
// compares. A full clear is the only repair that does not depend on knowing
// which one it was. The capture is taken FIRST, so the mangled screen is
// what gets recorded.
//
// What is reset:
//   - the terminal's modes, written straight to the tty: alt screen, SGR,
//     the G0 charset, scroll margins, origin and insert mode, autowrap — the
//     state a stray printf leaves behind and Bubble Tea never re-asserts;
//   - the scroll region (handed back, then synced from scratch), and the
//     renderer's line cache (ClearScreen), so every row is written again;
//   - the terminal size, asked for again (a missed SIGWINCH);
//   - holdConvo's padding, a mouse highlight, the wrap and viewport caches;
//   - mouse mode (as /mouse last set it), bracketed paste, a hidden cursor.
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

// repaintDoneMsg arrives after the clears: the region may take its rows again.
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
	dir, err := m.capture(time.Now())
	return m.repaint(repaintDoneMsg{dir: dir, err: err, report: report})
}

// repaint drops every cache and hold between dun's state and the screen and
// returns the commands that clear and redraw it. In a Sequence, because the
// order is the point: the region must not sync before the clears have run
// (minRegionGap explains why Cmds otherwise race).
func (m *tuiModel) repaint(done repaintDoneMsg) tea.Cmd {
	m.convoHold, m.convoHoldW = 0, 0
	m.drag = mouseSel{}
	if m.sr != nil {
		off := m.sr.off
		*m.sr = scrollRegion{off: off, wait: true}
	}
	for i := range m.convo {
		m.convo[i].invalidateWrap()
	}
	if m.vpc != nil {
		m.vpc.valid = false
	}
	if m.w > 0 && m.h > 0 {
		m.refresh()
	}
	mouse := tea.EnableMouseAllMotion
	switch m.mouseMode {
	case "cell":
		mouse = tea.EnableMouseCellMotion
	case "off":
		mouse = tea.DisableMouse
	}
	//nolint:staticcheck // SA1019: deprecated, but v1.3.10 has no replacement - the only scroll-region mechanism
	clearRegion := tea.ClearScrollArea
	return tea.Sequence(
		writeTermReset,
		clearRegion,
		tea.ClearScreen,
		tea.WindowSize(),
		mouse,
		tea.EnableBracketedPaste,
		tea.HideCursor,
		func() tea.Msg { return done },
	)
}

// writeTermReset writes termReset to the tty. A Cmd, so it runs in the
// Sequence's order; its write is one syscall, so it lands between two of the
// renderer's frames, never inside one, and each frame starts by homing the
// cursor itself.
func writeTermReset() tea.Msg {
	if isTTY(os.Stdout) {
		_, _ = os.Stdout.WriteString(termReset)
	}
	return nil
}

// repaintDone runs once the clears are through.
func (m *tuiModel) repaintDone(msg repaintDoneMsg) {
	if m.sr != nil {
		m.sr.wait = false
	}
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
// after keeping its last h lines and cutting each to the width; and while the
// scroll region is live, the rows it owns are blank filler in View
// (blankRegion) and the terminal shows what the region last drew (sr.last).
// So: the last frame, as the renderer cut it, with the region's rows laid over
// it.
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
	if sr := m.sr; sr != nil && sr.live {
		for i, line := range sr.last {
			if r := sr.top - 1 + i; r >= 0 && r < len(rows) && r <= sr.bot-1 {
				rows[r] = line
			}
		}
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
		expTxt[i] = ansi.Strip(r)
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
			actTxt[i] = ansi.Strip(r)
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
expected.ansi  terminal size, with the scroll region's rows laid over it (.txt
               without escape codes, .ansi with them)
actual.txt     what the terminal shows, from tmux capture-pane (or why it is
actual.ansi    not available: outside tmux there is no way to read it back)
diff.txt       the rows where expected and actual differ, both versions
state.json     sizes (dun, the tty, tmux), terminal env, mouse mode, layout
               numbers, the scroll region, focus, open pickers, holds, the
               dun version and the session id
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
	Kitty   bool              `json:"kitty_keyboard"`
	Mouse   string            `json:"mouse"`
	Layout  captureLayout     `json:"layout"`
	Region  captureRegion     `json:"scroll_region"`
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

type captureRegion struct {
	Off   bool `json:"off"`
	Live  bool `json:"live"`
	Held  bool `json:"held"`
	Top   int  `json:"top"`
	Bot   int  `json:"bot"`
	PaneH int  `json:"pane_height"`
	Rows  int  `json:"rows"`
}

func (m tuiModel) captureState(now time.Time) captureState {
	st := captureState{
		Time:    now.Format(time.RFC3339),
		Version: version,
		Session: m.sessionID,
		Env:     map[string]string{},
		Kitty:   kittyFD >= 0,
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
	if sr := m.sr; sr != nil {
		st.Region = captureRegion{Off: sr.off, Live: sr.live, Held: sr.held, Top: sr.top, Bot: sr.bot, PaneH: sr.paneH, Rows: len(sr.last)}
	}
	return st
}

// openName names what has the keys besides the composer.
func (m tuiModel) openName() string {
	switch {
	case m.inspecting:
		return "inspector"
	case m.paging:
		return "pager"
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
type eventRing struct {
	buf  [evlogSize]evRec
	next int // where the next one goes
	n    int // how many are held (≤ evlogSize)
}

type evRec struct {
	at   time.Time
	kind string
	info string
}

func newEventRing() *eventRing { return &eventRing{} }

// add records one message. kind is msgKind's (computed once per Update).
func (r *eventRing) add(at time.Time, kind string, msg tea.Msg) {
	if r == nil {
		return
	}
	r.buf[r.next] = evRec{at: at, kind: kind, info: msgSummary(msg)}
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
		line, _ := json.Marshal(struct {
			T    string `json:"t"`
			Kind string `json:"kind"`
			Info string `json:"info,omitempty"`
		}{e.at.Format("15:04:05.000"), e.kind, e.info})
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
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes {
			if msg.Paste {
				return "paste " + strconv.Itoa(len(msg.Runes)) + " runes"
			}
			return "rune"
		}
		return msg.String()
	case tea.MouseMsg:
		return msg.String() + " " + strconv.Itoa(msg.X) + "," + strconv.Itoa(msg.Y)
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
