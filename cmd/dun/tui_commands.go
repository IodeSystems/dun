package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/buildinfo"
)

// mouseSlash switches mouse reporting at runtime.
//
// It exists because Termux forces a choice dun cannot make for you. All-motion
// (1003, the default, and what claude runs) leaves Termux's mouse tracking
// INACTIVE, so a tap raises the soft keyboard — and a finger-scroll arrives as
// plain ↑/↓ keys, which scroll the conversation a row at a time. Cell motion
// (1002) makes Termux track the pointer, so the wheel scrolls the log directly
// but a tap never opens the keyboard. On a desktop terminal the difference is
// only whether motion events are reported at all.
func mouseSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) == 0 {
		m.append(stDim.Render("mouse: " + m.mouseMode + "  (all|cell|off)\n" +
			"  click selects a block or focuses the input · drag copies the text (OSC 52; tmux's buffer too)\n" +
			"  the terminal's own selection: shift+drag (iTerm2: option+drag)\n" +
			"  tmux taking drags for copy-mode? bind -n MouseDrag1Pane if -F '#{mouse_any_flag}' 'send -M' 'copy-mode -M'"))
		return nil
	}
	switch args[0] {
	case "all":
		m.mouseMode = "all"
		m.append(stDim.Render("mouse: all-motion — tap-to-type on Termux; ↑/↓ scroll the log"))
		return nil
	case "cell":
		m.mouseMode = "cell"
		m.append(stDim.Render("mouse: cell-motion — wheel scrolls the log; no tap-to-type on Termux"))
		return nil
	case "off":
		m.mouseMode = "off"
		m.append(stDim.Render("mouse: off — the terminal keeps the pointer (native selection)"))
		return nil
	}
	m.append(stErr.Render("mouse: want all|cell|off, got " + args[0]))
	return nil
}

// argPreview is a one-line `key=value` summary of a call's args (each value
// clipped), for the collapsed call line — so you SEE the input, not just keys.
func argPreview(args map[string]any, max int) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, k := range sortedKeys(args) {
		parts = append(parts, k+"="+clip(oneLine(fmt.Sprint(args[k])), 48))
	}
	return clip(strings.Join(parts, ", "), max)
}

// callText is the one-line form of a tool call — `server · tool  k=v, …` — for the
// collapsed block and the status line.
//
// exec is the exception: a shell command reads as a shell command, so it comes
// out as `$ go test ./...` and the `exec(command=` wrapper, which tells a reader
// nothing they did not already know, is dropped. Any OTHER arg (timeout) still
// shows, in parentheses after the command.
func callText(tool string, args map[string]any, max int) string {
	if line, ok := execCallText(args, 40); ok && tool == "exec" {
		return clip(oneLine(line), max)
	}
	if line, ok := evalCallText(args, max); ok && tool == "eval" {
		return line
	}
	if line, ok := fileCallText(tool, args, max); ok {
		return line
	}
	line := toolLabel(tool)
	if p := argPreview(args, max); p != "" {
		line += "  " + p
	}
	return clip(line, max)
}

// toolLabel is a tool's name as a person reads it: "notes__project_notes"
// is "notes · project notes", "node_query" is "node query". The one place a
// tool name is turned into words for the screen; the model, the logs and the
// inspector keep the real name.
func toolLabel(tool string) string {
	words := func(s string) string {
		return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '_' }), " ")
	}
	if srv, name, ok := strings.Cut(tool, "__"); ok && srv != "" && name != "" {
		return srv + " · " + words(name)
	}
	if w := words(tool); w != "" {
		return w
	}
	return tool
}

// execCallText renders exec's args as a shell prompt line. ok=false when there
// is no usable command, so the caller falls back to the generic form.
func execCallText(args map[string]any, restMax int) (string, bool) {
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return "", false
	}
	rest := make(map[string]any, len(args))
	for k, v := range args {
		if k != "command" {
			rest[k] = v
		}
	}
	line := "$ " + cmd
	if p := argPreview(rest, restMax); p != "" {
		line += "  (" + p + ")"
	}
	return line, true
}

// callFull is the expanded call line: the tool name over its args, or — for
// exec — the command verbatim, newlines and all, since a multi-line script is
// exactly what you expanded the block to read.
func callFull(tool string, args map[string]any) string {
	if line, ok := execCallText(args, 200); ok && tool == "exec" {
		return stTool.Render(line)
	}
	if tool == "eval" && evalCode(args) != "" {
		return evalCallFull(args)
	}
	full := stTool.Render(toolLabel(tool))
	if af := argFull(args); af != "" {
		full += "\n" + af
	}
	return full
}

// argFull renders the call's args in full (multi-line values kept intact), shown
// when the tool block is expanded.
func argFull(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range sortedKeys(args) {
		b.WriteString(stDim.Render("  "+k+":") + " " + fmt.Sprint(args[k]) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── slash commands ─────────────────────────────────────────────────
//
// Input beginning with "/" is a local command (never sent to the engine).
// Typing "/" opens a live PALETTE (see palettePanel) listing matching commands
// with descriptions; ↑/↓ selects, tab completes, enter runs. New commands go in
// slashCommands — the palette + /help pick them up automatically.

type slashCmd struct {
	name, args, desc string
	run              func(m *tuiModel, args []string) tea.Cmd
}

// Populated in init() (not a var literal) — the help handler reads
// slashCommands, which would be a static initialization cycle otherwise.
var slashCommands []slashCmd

func init() {
	slashCommands = []slashCmd{
		{"login", "[name]", "connect to an AI service: its address, and a key if it needs one", loginSlash},
		{"model", "[provider] [name] [--session] | use <provider> <id>", "pick the model: a searchable list from every service you are logged in to", modelSlash},
		// /mcp add is a subcommand, listed on its own so a person who was
		// given a server finds it in the first screen of the list (run 4:
		// it was only in /mcp's description).
		{"mcp add", "", "connect a server you were given — its command or address", func(m *tuiModel, args []string) tea.Cmd {
			return engineSlash("mcp")(m, append([]string{"add"}, args...))
		}},
		{"help", "[command|--debug]", "list these commands; /help <command> says one in full", func(m *tuiModel, args []string) tea.Cmd {
			switch {
			case len(args) > 0 && args[0] == "--debug":
				m.showHelp(true)
			case len(args) > 0:
				m.showCommandHelp(args[0])
			default:
				m.showHelp(false)
			}
			return nil
		}},
		{"config", "", "show this session's model settings (change with `dun --setup`)", func(m *tuiModel, _ []string) tea.Cmd { m.showConfig(); return nil }},
		{"status", "", "this session: build, workspace, model, tools and servers", func(m *tuiModel, _ []string) tea.Cmd { m.statusSlash(); return nil }},
		{"logout", "<provider>", "log out of an AI service: forget its address and key", logoutSlash},
		{"context", "[config [--max N] [--compact-buffer N] [--tool-lod N] [--recap-nudge N]]", "inspect context usage: tokens, compaction, recaps, truncation; config: the window, the reply's buffer, tool-result stubs and the recap reminder, saved for this model", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) > 0 && strings.EqualFold(args[0], "config") {
				// Bare: the picker (the flags with their values, contextChoices).
				if len(args) == 1 {
					m.input.SetValue("/context config ")
					m.input.CursorEnd()
					m.paletteSel = 0
					return nil
				}
				if !m.proc.controlCmd("ctxconfig", strings.Join(args[1:], " ")) {
					m.append(stErr.Render("no engine right now — /reconnect first"))
				}
				return nil
			}
			m.showContext()
			return nil
		}},
		{"reload", "", "restart this TUI into the dun binary on disk (after make install), on the same session", func(m *tuiModel, _ []string) tea.Cmd {
			m.reloadReq = true
			return tea.Quit
		}},
		{"upgrade", "[now|off|on]", "dund's pending upgrade: what it waits for; now restarts it at once (cut work resumes after); off|on: a release build's daily update check", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) > 0 && (args[0] == "off" || args[0] == "on") {
				line, err := upgradeSwitch(args[0] == "on")
				if err != nil {
					m.append(stErr.Render("updates: not saved: " + err.Error()))
					return nil
				}
				m.append(stDim.Render(line))
				return nil
			}
			if m.upgrade == nil {
				// A source build: nothing tells dund there is a newer build,
				// so dund and its
				// engines keep the one they started with. now moves them to
				// the build this TUI runs (USER 2026-10-27: /upgrade on a
				// source build was a dead end).
				if len(args) > 0 && args[0] == "now" {
					m.append(stDim.Render("asking dund to restart into this build (" + buildinfo.Version + ") — running turns are interrupted and resume after"))
					return upgradeIntoSelfCmd(m.proc)
				}
				line := "no dund upgrade is pending"
				if !buildinfo.IsRelease() {
					line += " · dund and its engines keep the build they started with; /upgrade now restarts them into this one (" + buildinfo.Version + ")"
				}
				m.append(stDim.Render(line))
				return nil
			}
			if len(args) == 0 || args[0] != "now" {
				m.append(stNote.Render(upgradeDetail(m.upgrade, m.sessionID)))
				return nil
			}
			if !m.proc.upgradeNow() {
				m.append(stErr.Render("no session service right now — nothing restarted"))
				return nil
			}
			m.append(stDim.Render("asked dund to restart now — running turns are interrupted and resume after"))
			return nil
		}},
		{"version", "", "which dun this is, the newest release, and whether this build updates itself, and from where", versionSlash},
		{"keys", "[reload|preset <name>]", "the key map: every rebindable action and its keys; yours go in ~/.dun/keys.json", keysSlash},
		{"perf", "", "redraw timings for this session (and how to get a profile)", func(m *tuiModel, _ []string) tea.Cmd {
			m.append(stHeader.Render("render performance") + "\n" + frames.report())
			return nil
		}},
		{"repaint", "[report]", "clear and redraw the whole screen, after saving what dun drew and what the terminal shows to ~/.dun/bugreports/; report also has the model redact the capture and explain what went wrong (asks first if the model is not local)", repaintSlash},
		{"mouse", "[all|cell|off]", "mouse reporting mode (bare shows it) — `cell` if the wheel should scroll the log; click selects, drag copies, shift+drag (iTerm2: option+drag) is the terminal's own selection", mouseSlash},
		{"scrolldebug", "", "debug: dump scroll overlay state (rowOffset, blockH, YOffset)", func(m *tuiModel, _ []string) tea.Cmd {
			var lines []string
			lines = append(lines, fmt.Sprintf("YOffset=%d pinned=%v vpH=%d convo=%d blockH=%d",
				m.vp.YOffset, m.scrollPinned, m.vp.Height, len(m.convo), len(m.frame.blockH)))
			for i, e := range m.convo {
				if e.userText == "" {
					continue
				}
				h := 0
				if i < len(m.frame.blockH) {
					h = m.frame.blockH[i]
				}
				lines = append(lines, fmt.Sprintf("  [%d] user=%q rowOffset=%d h=%d bottom=%d offScreen=%v",
					i, e.userText, e.rowOffset, h, e.rowOffset+h, e.rowOffset+h <= m.vp.YOffset))
			}
			m.append(stDim.Render(strings.Join(lines, "\n")))
			return nil
		}},
		{"resume", "[id]", "switch to another saved session (bare opens the picker)", resumeSlash},
		{"new", "", "start a fresh session in this workspace", newSlash},
		{"trace", "[on|off|file]", "record events+scroll to a file for debugging (default: dun-trace.jsonl in the temp dir)", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) == 0 || args[0] == "on" {
				// Not the workspace by default: dun writes nothing into a repo
				// it was not asked to. A path given is the human's choice.
				path := filepath.Join(os.TempDir(), "dun-trace.jsonl")
				if len(args) >= 2 {
					path = args[1]
					if !filepath.IsAbs(path) {
						path = filepath.Join(m.workspace, path)
					}
				}
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
				if err != nil {
					m.append(stErr.Render("trace: " + err.Error()))
					return nil
				}
				if m.traceFile != nil {
					m.traceFile.Close()
				}
				m.traceFile = f
				m.tracePrevYOff = -1
				// Dump the current conversation layout so we can replay without
				// needing the history event (trace doesn't survive /reload re-exec).
				fmt.Fprintf(f, "resize w=%d h=%d\n", m.w, m.h)
				for i, e := range m.convo {
					h := 0
					if i < len(m.frame.blockH) {
						h = m.frame.blockH[i]
					}
					kind := "assistant"
					if e.userText != "" {
						kind = "user"
					} else if e.run != nil {
						kind = "tool"
					}
					fmt.Fprintf(f, "layout %d kind=%s userText=%q rowOffset=%d h=%d\n",
						i, kind, e.userText, e.rowOffset, h)
				}
				m.append(stDim.Render("tracing → " + filepath.Join(m.workspace, path)))
				return nil
			}
			if args[0] == "off" {
				if m.traceFile != nil {
					m.traceFile.Close()
					m.traceFile = nil
				}
				m.append(stDim.Render("tracing off"))
				return nil
			}
			m.append(stDim.Render("usage: /trace [on|off [file]]"))
			return nil
		}},
		{"reconnect", "", "restart the engine (after it gave up), keeping this session", func(m *tuiModel, _ []string) tea.Cmd {
			m.restarts, m.restartStart = 0, time.Now()
			m.reconnTries, m.reconnSince = 0, time.Time{}
			m.fatalErr = ""
			m.exitAnnounced = false
			m.skipHistory = m.everUp
			m.append(stDim.Render("reconnecting…"))
			return restartEngine(m.opts, m.sessionID, m.everUp, m.seen())
		}},
		{"skill", "[name] [text] | on|off|unset <name|glob> [--global|--project|--session] | mode listed|surfaced|unset [--scope]", "skills: bare opens the list (~/.dun/skills, .dun/skills, ~/.claude/skills, MCP servers) — type to filter, each one's cost, on/off at each scope, and the mode (listed in every request, or surfaced when a turn fits); on/off/unset and mode change it directly; with a name, asks the model to use it; with part of a name, lists the matches", skillSlash},
		{"exit", "", "exit dun: the session's engine stops too, unless /persist is on or another client is attached", func(m *tuiModel, _ []string) tea.Cmd {
			return m.exit()
		}},
		// /splash: a clean screen with the splash on it (USER 2026-10-27:
		// "it's fun"). The conversation is the engine's and stays; only what
		// this screen shows is cleared, and a /resume shows it again.
		{"splash", "", "show the splash again at the top of a clear screen (what was above is still there, scrolled back)", func(m *tuiModel, _ []string) tea.Cmd {
			m.sel, m.paging, m.scr = -1, false, nil
			m.splashAgain()
			return nil
		}},
		// /close exits, as /exit (USER 2026-10-27); discarding a session for
		// good is /discard.
		{"close", "", "exit dun, as /exit (to discard the session for good: /discard)", func(m *tuiModel, _ []string) tea.Cmd {
			return m.exit()
		}},
		{"persist", "[on|off|status]", "keep this session running after /exit (bare: on, or the status if already on)", func(m *tuiModel, args []string) tea.Cmd {
			mode := ""
			if len(args) > 0 {
				mode = strings.ToLower(args[0])
			}
			switch mode {
			case "", "on", "off", "status":
			default:
				m.append(stErr.Render("usage: /persist [on|off|status]"))
				return nil
			}
			if !m.proc.persist(mode) {
				m.append(stErr.Render("no session service right now — /reconnect, then try again"))
			}
			return nil
		}},
	}
}

// paletteActive reports whether the "/" command palette should be shown/driven.
func (m tuiModel) paletteActive() bool {
	return m.focus == focusInput && !m.asking && strings.HasPrefix(m.input.Value(), "/")
}

// paletteMatches returns the commands whose name starts with the typed word.
func (m tuiModel) paletteMatches() []slashCmd {
	word := strings.TrimPrefix(strings.Fields(m.input.Value() + " ")[0], "/")
	typed := strings.TrimPrefix(strings.TrimSpace(m.input.Value()), "/")
	var out []slashCmd
	for _, c := range m.allSlash() {
		if strings.Contains(c.name, " ") {
			// A subcommand listed on its own (/mcp add): matched by all the
			// words typed, so "/mcp tools" is not it.
			if strings.HasPrefix(c.name, typed) || strings.HasPrefix(typed, c.name+" ") {
				out = append(out, c)
			}
			continue
		}
		if debugCommands[c.name] && c.name != word {
			continue // a debug command shows once its whole name is typed
		}
		if strings.HasPrefix(c.name, word) {
			out = append(out, c)
		}
	}
	return out
}

// runSlash dispatches "/name args…" by exact name or a unique prefix.
func (m *tuiModel) runSlash(v string) tea.Cmd {
	m.slashLine = v
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return nil
	}
	name := strings.TrimPrefix(fields[0], "/")
	var hit *slashCmd
	n := 0
	cmds := m.allSlash()
	for i := range cmds {
		if strings.Contains(cmds[i].name, " ") {
			continue // a subcommand's own palette row: /mcp runs /mcp
		}
		if cmds[i].name == name { // exact wins outright
			return cmds[i].run(m, fields[1:])
		}
		if strings.HasPrefix(cmds[i].name, name) && !debugCommands[cmds[i].name] {
			hit = &cmds[i]
			n++
		}
	}
	if n == 1 {
		return hit.run(m, fields[1:])
	}
	if n == 0 {
		// Not one of this TUI's: the engine has the last word (its list may
		// not have arrived yet). It answers an unknown one itself.
		return engineSlash(name)(m, fields[1:])
	}
	m.append(stDim.Render("ambiguous command: /" + name + " — try /help"))
	return nil
}

// runPaletteEnter runs the highlighted palette command (preserving any typed
// args), or the exactly-typed command.
func (m *tuiModel) runPaletteEnter(v string) tea.Cmd {
	// An argument choice highlighted: run the command with it.
	if cmd, l, ok := m.paletteArgs(); ok && len(l.choices) > 0 {
		sel := min(max(m.paletteSel, 0), len(l.choices)-1)
		m.paletteSel = 0
		// A choice that wants a value after it (a flag: --max) is filled in,
		// as tab does, not run without one (USER 2026-10-27: enter on --max
		// ran "--max" and said it needs a value).
		if l.choices[sel].more {
			m.input.SetValue("/" + cmd + " " + l.choices[sel].value + " ")
			m.input.CursorEnd()
			m.refresh()
			return nil
		}
		m.input.Reset()
		return m.runSlash("/" + cmd + " " + l.choices[sel].value)
	}
	fields := strings.Fields(v)
	matches := m.paletteMatches()
	sel := m.paletteSel
	m.input.Reset()
	m.paletteSel = 0
	if sel < 0 || sel >= len(matches) {
		sel = 0
	}
	// If the first word is already an exact command, honor it (with args).
	if len(fields) > 0 {
		if word := strings.TrimPrefix(fields[0], "/"); m.commandNamed(word) {
			return m.runSlash(v)
		}
	}
	if len(matches) == 0 {
		// None of the known ones: the engine decides (runSlash hands it on).
		return m.runSlash(v)
	}
	if c := matches[sel]; strings.Contains(c.name, " ") {
		// A subcommand's row. Typed as the command's word alone and not
		// chosen with the arrows (/mcp, enter), it is the command itself.
		first := strings.Fields(c.name)[0]
		if sel == 0 && len(fields) == 1 && strings.TrimPrefix(fields[0], "/") == first {
			return m.runSlash(v)
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "/"), c.name)
		if rest == strings.TrimPrefix(strings.TrimSpace(v), "/") {
			rest = "" // the name was not typed out: no arguments
		}
		return c.run(m, strings.Fields(rest))
	}
	args := ""
	if len(fields) > 1 {
		args = " " + strings.Join(fields[1:], " ")
	}
	return m.runSlash("/" + matches[sel].name + args)
}
func (m tuiModel) commandNamed(name string) bool {
	for _, c := range m.allSlash() {
		if c.name == name {
			return true
		}
	}
	return false
}

// showHelp appends the command list to the conversation: without debug, the
// debug commands left out and counted on the last row.
func (m *tuiModel) showHelp(debug bool) {
	var b strings.Builder
	b.WriteString(stHeader.Render("commands"))
	hidden := 0
	for _, c := range m.allSlash() {
		if debugCommands[c.name] && !debug {
			hidden++
			continue
		}
		usage := "/" + c.name
		if c.args != "" {
			usage += " " + c.args
		}
		b.WriteString("\n  " + stCmd.Render(usage) + "  " + stDim.Render(c.desc))
	}
	if hidden > 0 {
		b.WriteString("\n\n  " + stDim.Render(plural(hidden, "debug command")+" hidden — /help --debug shows them"))
	}
	m.append(b.String())
}

// showCommandHelp is /help <command>: its usage, what it does, the long help
// where the command has one, and the values its first argument takes.
func (m *tuiModel) showCommandHelp(name string) {
	name = strings.TrimPrefix(name, "/")
	var c *slashCmd
	for _, s := range m.allSlash() {
		if s.name == name {
			c = &s
			break
		}
	}
	if c == nil {
		m.append(stErr.Render("no command /" + name + " — /help lists them"))
		return
	}
	var ec *engineCmd
	for i := range m.engineCmds {
		if m.engineCmds[i].name == name {
			ec = &m.engineCmds[i]
		}
	}
	usage := "/" + c.name
	if c.args != "" {
		usage += " " + c.args
	}
	rows := []string{stCmd.Render("/"+c.name) + "  " + c.desc}
	if ec != nil && ec.help != "" {
		rows = append(rows, "")
		for _, l := range strings.Split(ec.help, "\n") {
			rows = append(rows, "  "+l)
		}
	}
	if ec != nil && len(ec.choices) > 0 {
		rows = append(rows, "", stDim.Render("  "+usage))
		w := 0
		for _, ch := range ec.choices {
			w = max(w, len(ch.value))
		}
		for _, ch := range ec.choices {
			rows = append(rows, "  "+stCmd.Render(fmt.Sprintf("%-*s", w, ch.value))+"  "+stDim.Render(ch.desc))
		}
	} else if c.args != "" {
		rows = append(rows, stDim.Render("  "+usage))
	}
	m.append(strings.Join(rows, "\n"))
}

// palettePanel renders the live command list above the input (like the ask
// picker), the highlighted row gutter-marked.
func (m tuiModel) palettePanel() string {
	if cmd, l, ok := m.paletteArgs(); ok {
		return m.argPanel(cmd, l) + "\n" + m.input.View()
	}
	matches := m.paletteMatches()
	lo, hi := paletteRange(m.paletteSel, len(matches))
	rows := make([]string, 0, hi-lo+2)
	// A heading, as every panel below the conversation has one.
	rows = append(rows, stHeader.Render("commands"))
	// The descriptions in one column: the names padded to the widest one
	// shown. The arguments are on the row below the list, for the selected
	// command only — inline they pushed every description to a different
	// column.
	nameW := 0
	for _, c := range matches[lo:hi] {
		nameW = max(nameW, len(c.name)+1)
	}
	for i := lo; i < hi; i++ {
		c := matches[i]
		pad := strings.Repeat(" ", nameW-len(c.name)-1)
		name := stCmd.Render("/" + c.name)
		if i == m.paletteSel {
			name = "/" + c.name // listRow puts it in the accent
		}
		// A description too long for the row ends at a clause, so what
		// is left still says something (fitClause).
		desc := fitClause(c.desc, m.w-len(listIndent)-nameW-2)
		rows = append(rows, listRow("", name, pad+"  "+stDim.Render(desc), i == m.paletteSel))
	}
	// The count first: a usage too long for the row ends in an ellipsis.
	var foot []string
	if len(matches) == 0 {
		foot = append(foot, "no matching command · /help")
	} else if lo > 0 || hi < len(matches) {
		foot = append(foot, fmt.Sprintf("%d-%d of %d commands", lo+1, hi, len(matches)))
	}
	if m.paletteSel >= 0 && m.paletteSel < len(matches) && matches[m.paletteSel].args != "" {
		c := matches[m.paletteSel]
		foot = append(foot, "/"+c.name+" "+c.args)
	}
	if len(foot) > 0 {
		// A blank row first: the count is about the list, not a row of it
		// (run 5: list, count, input and hints stacked with no gap).
		rows = append(rows, "", stDim.Render("  "+strings.Join(foot, "  ·  ")))
	}
	return fitRows(strings.Join(rows, "\n"), m.w) + "\n" + m.input.View()
}
