package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
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
)

// ── model ──────────────────────────────────────────────────────────

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
	asking     bool   // agent is waiting on an ask_user answer
	askOptions []string
	md         *glamour.TermRenderer // markdown renderer for assistant replies
	history    []string              // sent inputs, for up/down recall
	histIdx    int                   // cursor into history (== len when not browsing)
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
	return tuiModel{proc: proc, workspace: workspace, input: in, spin: sp, starting: true}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.proc.ch), m.spin.Tick, textinput.Blink)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp = viewport.New(msg.Width, msg.Height-4)
		m.input.Width = msg.Width - 2
		m.md = newMarkdown(msg.Width - 2)
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "enter":
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				return m, nil
			}
			// Answering an ask_user: a number picks an option; else free text.
			if m.asking {
				ans := v
				if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(m.askOptions) {
					ans = m.askOptions[n-1]
				}
				m.input.Reset()
				m.append(stUser.Render("› " + ans))
				m.asking = false
				m.proc.answer(ans)
				return m, nil
			}
			if m.busy || m.starting {
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
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
			}
			return m, nil
		case "down":
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
		m.asking = true
		m.append(stAsk.Render("❓ " + str(ev["question"])))
		m.askOptions = nil
		if opts, ok := ev["options"].([]any); ok {
			for i, o := range opts {
				m.askOptions = append(m.askOptions, fmt.Sprint(o))
				m.append(stDim.Render(fmt.Sprintf("   %d) %s", i+1, fmt.Sprint(o))))
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
	rule := stDim.Render(strings.Repeat("─", max(1, m.w)))
	var status string
	switch {
	case m.fatalErr != "":
		status = stErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + stDim.Render(" spawning tool servers…")
	case m.asking:
		status = stAsk.Render("❓ waiting for your answer (a number picks an option)")
	case m.busy:
		status = m.spin.View() + stDim.Render(" working…  (ctrl+c to quit)")
	default:
		status = stDim.Render("ready  ·  ↑/↓ history · pgup/pgdn scroll · ctrl+c quit")
	}
	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s", head, m.vp.View(), rule, m.input.View(), status)
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
	lines := m.convo
	if m.cur != "" {
		lines = append(append([]string{}, m.convo...), m.cur)
	}
	m.vp.SetContent(lipgloss.NewStyle().Width(m.vp.Width).Render(strings.Join(lines, "\n")))
	m.vp.GotoBottom()
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
