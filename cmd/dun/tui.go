package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// The TUI is a CLIENT of the `-p` JSON event protocol: it re-execs `dun -p`,
// writes user events to its stdin, and renders the event stream. The engine
// stays headless; the UI is pure presentation.

// tuiOpts are the flags the TUI forwards to its `dun -p` subprocess.
type tuiOpts struct {
	workspace, model, url, key, docker string
	noWorktree                         bool
	pr                                 bool
	cont                               bool   // --continue: resume the latest session
	resume                             string // --resume <id>: resume a specific session
}

// runTUI launches the Bubble Tea app against a re-exec'd `dun -p` subprocess.
func runTUI(o tuiOpts) error {
	proc, err := startDunProc(o)
	if err != nil {
		return err
	}
	m := newTUIModel(proc, o.workspace)
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	proc.close()
	return err
}

// ── styles ─────────────────────────────────────────────────────────

var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stUser   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	stTool   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	stNote   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // proactive notifications
	stAsk    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	stSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")) // selection gutter
)

// paneStyle borders a pane; the focused one is bright (212), else dim (240) —
// the tmux split-pane look (the bright border is the focused pane's half-edge).
func paneStyle(focused bool) lipgloss.Style {
	c := lipgloss.Color("240")
	if focused {
		c = lipgloss.Color("212")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c)
}

// addGutter prefixes every line of a (possibly multi-line) block with marker,
// so a highlighted message/option shows a left border down its whole height.
func addGutter(block, marker string, style lipgloss.Style) string {
	pre := style.Render(marker)
	lines := strings.Split(block, "\n")
	for i, ln := range lines {
		lines[i] = pre + ln
	}
	return strings.Join(lines, "\n")
}

// ── model ──────────────────────────────────────────────────────────

// Focus is which pane keys drive — tmux-style: Tab toggles, and the focused
// pane wears a bright border. In convo focus, ↑/↓ move a message selection
// (left-border highlight) instead of recalling input history.
const (
	focusInput = iota // typing + ↑/↓ history (default)
	focusConvo        // ↑/↓ select a message, viewport follows
)

type tuiModel struct {
	proc      *dunProc
	workspace string
	vp        viewport.Model
	input     textinput.Model
	spin      spinner.Model
	convo     []string // finalized conversation lines
	cur       string   // streaming assistant text (not yet finalized); string, not
	//                    strings.Builder — Bubble Tea copies the model each Update.
	tools      []string
	branch     string // worktree branch (from the `workspace` event)
	starting   bool   // spawning servers, before `ready`
	busy       bool   // a turn in flight
	asking       bool     // agent is waiting on an ask_user answer
	askOptions   []string // the offered options; a trailing "custom" row is implicit
	askSel       int      // highlighted answer row (== len(askOptions) → the custom row)
	askNote      string   // optional detail attached to the chosen option ("n")
	noting       bool     // capturing a detail for the selected option
	customAnswer bool     // capturing a free-text / chat answer
	md           *glamour.TermRenderer // markdown renderer for assistant replies
	history    []string              // sent inputs, for up/down recall
	histIdx    int                   // cursor into history (== len when not browsing)
	focus      int                   // focusInput | focusConvo
	sel        int                   // selected message index (convo focus); -1 = none
	w, h       int
	fatalErr   string
}

func newTUIModel(proc *dunProc, workspace string) tuiModel {
	in := textinput.New()
	in.Placeholder = "ask dun to do something…"
	in.Prompt = "› "
	in.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return tuiModel{proc: proc, workspace: workspace, input: in, spin: sp, starting: true, sel: -1}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.proc.ch), m.spin.Tick, textinput.Blink)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		// Layout: head(1) + convo box(border 2 + content) + lower box(1+2) +
		// status(1). Convo content = h-7 in the normal (input) case; View
		// recomputes it when the lower pane grows (an ask panel).
		m.vp = viewport.New(max(1, msg.Width-2), max(1, msg.Height-7))
		m.input.Width = msg.Width - 4
		m.md = newMarkdown(msg.Width - 4)
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		// Answering an ask_user is a mode of its own (select an option, add a
		// detail, or type a custom/chat answer) — it owns the keys.
		if m.asking {
			return m.updateAsking(msg)
		}
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab":
			// Toggle focus between the input and the conversation (tmux-style).
			if m.focus == focusInput {
				m.focus = focusConvo
				m.input.Blur()
				if m.sel < 0 || m.sel >= len(m.convo) {
					m.sel = len(m.convo) - 1
				}
			} else {
				m.focus = focusInput
				m.input.Focus()
			}
			m.refresh()
			return m, textinput.Blink
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "enter":
			if m.focus == focusConvo {
				return m, nil
			}
			v := strings.TrimSpace(m.input.Value())
			if v == "" || m.busy || m.starting {
				return m, nil
			}
			m.history = append(m.history, v)
			m.histIdx = len(m.history)
			m.input.Reset()
			m.append(stUser.Render("› " + v))
			m.busy = true
			m.proc.send(v)
			return m, nil
		case "up":
			if m.focus == focusConvo { // move the message selection up
				if m.sel > 0 {
					m.sel--
				}
				m.refresh()
				return m, nil
			}
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
			}
			return m, nil
		case "down":
			if m.focus == focusConvo { // move the message selection down
				if m.sel < len(m.convo)-1 {
					m.sel++
				}
				m.refresh()
				return m, nil
			}
			if m.histIdx < len(m.history) {
				m.histIdx++
				if m.histIdx == len(m.history) {
					m.input.SetValue("")
				} else {
					m.input.SetValue(m.history[m.histIdx])
					m.input.CursorEnd()
				}
			}
			return m, nil
		default:
			if m.focus == focusConvo { // keys don't type into a blurred input
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

	case evMsg:
		return m.handleEvent(msg), waitEvent(m.proc.ch)

	case eofMsg:
		if m.fatalErr == "" {
			m.fatalErr = "dun engine exited"
		}
		m.busy, m.starting = false, false
		m.refresh()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateAsking drives the answer picker: ↑/↓ highlight an option (the last row
// is a custom/chat free-text answer), enter selects, `n` attaches a detail to
// the highlighted option. While typing a detail or a custom answer, keys go to
// the input; esc backs out of that sub-mode.
func (m tuiModel) updateAsking(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	custom := len(m.askOptions) // index of the trailing "custom / chat" row
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.noting || m.customAnswer {
			m.noting, m.customAnswer = false, false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		}
		return m, tea.Quit
	case "enter":
		switch {
		case m.noting:
			m.askNote = strings.TrimSpace(m.input.Value())
			m.noting = false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		case m.customAnswer:
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				return m, nil
			}
			return m.sendAnswer(v), nil
		case m.askSel == custom: // open free-text / chat entry
			m.customAnswer = true
			m.input.Reset()
			m.input.Placeholder = "type your answer, or chat…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		default:
			ans := m.askOptions[m.askSel]
			if m.askNote != "" {
				ans += " — " + m.askNote
			}
			return m.sendAnswer(ans), nil
		}
	case "up":
		if !m.noting && !m.customAnswer && m.askSel > 0 {
			m.askSel--
			m.refresh()
			return m, nil
		}
	case "down":
		if !m.noting && !m.customAnswer && m.askSel < custom {
			m.askSel++
			m.refresh()
			return m, nil
		}
	case "n":
		if !m.noting && !m.customAnswer && m.askSel < custom {
			m.noting = true
			m.input.Reset()
			m.input.Placeholder = "add a detail…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		}
	}
	if m.noting || m.customAnswer { // typing into the detail / custom field
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// sendAnswer resolves the ask: echoes the answer, ships it to the engine, and
// resets to the input pane.
func (m tuiModel) sendAnswer(v string) tuiModel {
	m.append(stUser.Render("› " + v))
	m.proc.answer(v)
	m.asking, m.noting, m.customAnswer = false, false, false
	m.askOptions, m.askSel, m.askNote = nil, 0, ""
	m.input.Reset()
	m.input.Placeholder = "ask dun to do something…"
	m.input.Focus()
	m.focus = focusInput
	m.refresh()
	return m
}

func (m tuiModel) handleEvent(ev evMsg) tuiModel {
	switch ev["type"] {
	case "workspace":
		m.branch = str(ev["branch"])
		m.append(stDim.Render("worktree branch: " + m.branch))
	case "ready":
		m.starting = false
		if ts, ok := ev["tools"].([]any); ok {
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.append(stDim.Render(fmt.Sprintf("ready — %d tools: %s", len(m.tools), strings.Join(m.tools, ", "))))
	case "token":
		m.busy = true // a turn is active (incl. autonomous background-completion turns)
		m.cur += str(ev["text"])
		m.refresh()
	case "tool_call":
		m.busy = true
		m.flushCur()
		m.append(stTool.Render("⚙ " + str(ev["tool"]) + "(" + argKeys(ev["args"]) + ")"))
	case "tool_result":
		res := str(ev["result"])
		if isDiff(res) {
			m.append(colorizeDiff(res)) // show the diff in full, colored
		} else {
			m.append(stDim.Render("  → " + clip(oneLine(res), 100)))
		}
	case "message":
		// tokens already streamed the reply; nothing to add.
	case "notification":
		m.append(stNote.Render("🔔 " + oneLine(str(ev["text"]))))
	case "ask":
		m.flushCur()
		m.asking, m.noting, m.customAnswer = true, false, false
		m.askSel, m.askNote = 0, ""
		m.input.Blur()
		m.append(stAsk.Render("❓ " + str(ev["question"])))
		m.askOptions = nil
		if opts, ok := ev["options"].([]any); ok {
			for _, o := range opts {
				m.askOptions = append(m.askOptions, fmt.Sprint(o))
			}
		}
	case "done":
		m.flushCur()
		m.busy = false
		m.refresh()
	case "error":
		m.append(stErr.Render("error: " + str(ev["error"])))
		m.busy = false
	}
	return m
}

func (m tuiModel) View() string {
	head := stHeader.Render("dun") + stDim.Render("  "+m.workspace)
	if m.branch != "" {
		head += stDim.Render("  ⎇ " + m.branch)
	}
	if n := len(m.tools); n > 0 {
		head += stDim.Render(fmt.Sprintf("  · %d tools", n))
	}

	// The lower pane is the input, or — while answering — the option picker. The
	// convo pane takes whatever height is left (the picker can be several rows).
	lower := m.lowerView()
	convoH := m.h - 4 - lipgloss.Height(lower) // head 1 + status 1 + convo border 2
	if convoH < 1 {
		convoH = 1
	}
	vp := m.vp
	vp.Height = convoH
	convoBox := paneStyle(m.focus == focusConvo && !m.asking).Width(max(1, m.w-2)).Render(vp.View())

	var status string
	switch {
	case m.fatalErr != "":
		status = stErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + stDim.Render(" spawning tool servers…")
	case m.asking:
		status = stAsk.Render("❓ ↑/↓ choose · enter select · n add detail · esc/ctrl+c quit")
	case m.busy:
		status = m.spin.View() + stDim.Render(" working…  (ctrl+c to quit)")
	case m.focus == focusConvo:
		status = stDim.Render("convo  ·  ↑/↓ select message · tab input · ctrl+c quit")
	default:
		status = stDim.Render("ready  ·  tab scroll · ↑/↓ history · ctrl+c quit")
	}
	return strings.Join([]string{head, convoBox, lower, status}, "\n")
}

// lowerView is the bottom pane: the input box, or the answer picker when asking.
func (m tuiModel) lowerView() string {
	if m.asking {
		return m.askPanel()
	}
	return paneStyle(m.focus == focusInput).Width(max(1, m.w-2)).Render(m.input.View())
}

// askPanel renders the answer options (highlighted selection), a trailing
// custom/chat row, any attached detail, and the text field when capturing one.
func (m tuiModel) askPanel() string {
	custom := len(m.askOptions)
	rows := make([]string, 0, custom+2)
	sel := func(i int) bool { return m.askSel == i && !m.noting && !m.customAnswer }
	gut := func(text string, on bool) string {
		if on {
			return addGutter(text, "▎ ", stSel)
		}
		return addGutter(text, "  ", lipgloss.NewStyle())
	}
	for i, opt := range m.askOptions {
		rows = append(rows, gut(opt, sel(i)))
	}
	rows = append(rows, gut(stDim.Render("✎ custom answer / chat…"), sel(custom) || m.customAnswer))
	if m.askNote != "" {
		rows = append(rows, stDim.Render("   detail: "+m.askNote))
	}
	if m.noting || m.customAnswer {
		rows = append(rows, m.input.View())
	}
	return paneStyle(true).Width(max(1, m.w-2)).Render(strings.Join(rows, "\n"))
}

// ── helpers ────────────────────────────────────────────────────────

func (m *tuiModel) append(line string) {
	m.convo = append(m.convo, line)
	m.refresh()
}

func (m *tuiModel) flushCur() {
	if m.cur != "" {
		// Finalize the streamed reply as rendered markdown (headers, lists, code).
		m.convo = append(m.convo, renderMarkdown(m.md, strings.TrimRight(m.cur, "\n")))
		m.cur = ""
	}
}

func (m *tuiModel) refresh() {
	blocks := m.convo
	if m.cur != "" {
		blocks = append(append([]string{}, m.convo...), m.cur)
	}
	// In convo focus, gutter every block (selected one bright) so the highlight
	// aligns and the selected message shows a left border down its full height.
	selMode := m.focus == focusConvo && !m.asking && len(blocks) > 0
	width := m.vp.Width
	if selMode {
		width -= 2 // reserve the gutter column (+ a space)
	}
	wrap := lipgloss.NewStyle().Width(max(1, width))
	rendered := make([]string, len(blocks))
	for i, b := range blocks {
		w := wrap.Render(b)
		if selMode {
			if i == m.sel {
				w = addGutter(w, "▎ ", stSel)
			} else {
				w = addGutter(w, "  ", lipgloss.NewStyle())
			}
		}
		rendered[i] = w
	}
	m.vp.SetContent(strings.Join(rendered, "\n"))

	// Keep the selection in view; otherwise stick to the bottom (live tail).
	if selMode && m.sel >= 0 && m.sel < len(rendered) {
		top := 0
		for i := 0; i < m.sel; i++ {
			top += lipgloss.Height(rendered[i])
		}
		h := lipgloss.Height(rendered[m.sel])
		switch {
		case top < m.vp.YOffset:
			m.vp.SetYOffset(top)
		case top+h > m.vp.YOffset+m.vp.Height:
			m.vp.SetYOffset(top + h - m.vp.Height)
		}
	} else {
		m.vp.GotoBottom()
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func argKeys(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

// ── subprocess (dun -p) ────────────────────────────────────────────

type evMsg map[string]any
type eofMsg struct{}

type dunProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	ch    chan tea.Msg
}

func startDunProc(o tuiOpts) (*dunProc, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"-p", "--workspace", o.workspace}
	if o.model != "" {
		args = append(args, "--model", o.model)
	}
	if o.url != "" {
		args = append(args, "--url", o.url)
	}
	if o.key != "" {
		args = append(args, "--key", o.key)
	}
	if o.docker != "" {
		args = append(args, "--docker", o.docker)
	}
	if o.noWorktree {
		args = append(args, "--no-worktree")
	}
	if o.pr {
		args = append(args, "--pr")
	}
	if o.cont {
		args = append(args, "--continue")
	}
	if o.resume != "" {
		args = append(args, "--resume", o.resume)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Engine stderr (mcp startup logs) → a temp log so it doesn't corrupt the UI.
	if f, err := os.CreateTemp("", "dun-tui-*.log"); err == nil {
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan tea.Msg, 256)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				ch <- evMsg(ev)
			}
		}
		ch <- eofMsg{}
	}()
	return &dunProc{cmd: cmd, stdin: stdin, ch: ch}, nil
}

func (p *dunProc) send(content string) {
	_ = json.NewEncoder(p.stdin).Encode(map[string]string{"type": "user", "content": content})
}

func (p *dunProc) answer(value string) {
	_ = json.NewEncoder(p.stdin).Encode(map[string]string{"type": "answer", "value": value})
}

func (p *dunProc) close() {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// waitEvent blocks for the next engine event and delivers it as a tea.Msg.
func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}
