package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The shell keys that act on the composer as a whole, or on the session,
// rather than on text (editcore.go has those). USER 2026-11-04 chose the
// shell's meaning where dun had another:
//
//	ctrl+p, ctrl+n        the previous / next message sent (ctrl+up/down too)
//	ctrl+c                with text in the composer, empties it (yank brings
//	                      it back); empty, it interrupts or quits as before
//	ctrl+l                redraw the screen
//	alt+<, alt+>          the start / end of the whole message
//	ctrl+x ctrl+e         edit the message in $VISUAL or $EDITOR
//	ctrl+x t              thinking: one line each, or in full (was ctrl+t,
//	                      which now swaps two characters)
//	ctrl+x p              paste the clipboard's text (was ctrl+p)
//
// ctrl+x starts a chord: the next key finishes it. A chord's keys are named
// "ctrl+x t" in keys.json.

// chordPrefix is the key that starts a chord.
const chordPrefix = "ctrl+x"

// chordActions are the actions a chord can be bound to, by name.
var chordActions = []string{"thinking.fold", "paste.text", "edit.external"}

// ctrlCClears says ctrl+c would empty the composer: it has the keys and text.
func (m tuiModel) ctrlCClears() bool {
	return m.focus == focusInput && m.input.Value() != "" &&
		!m.asking && !m.picking && !m.modelPicking && !m.mcpForm && m.rag == nil &&
		!m.providerForm && !m.loginPicking && !m.searching && !m.paging &&
		!m.jobViewing && !m.inspecting && m.activeScreen() == nil && m.confirm.q == ""
}

// composerKey takes the keys above, in the conversation and the composer.
// handled false gives the key to the rest of the switch; msg may come back
// as another key (ctrl+p is ctrl+up from there on).
func (m *tuiModel) composerKey(msg tea.KeyPressMsg) (tea.KeyPressMsg, tea.Cmd, bool) {
	key := msg.String()
	if m.chord != "" {
		chord := m.chord + " " + key
		m.chord = ""
		m.refresh()
		return msg, m.runChord(chord), true
	}
	if key == chordPrefix {
		m.chord = chordPrefix
		m.refresh()
		return msg, nil, true
	}
	if a := m.boundAction(key); a != "" { // a chord action rebound to one key
		return msg, m.runAction(a), true
	}
	switch key {
	case "ctrl+c":
		if m.ctrlCClears() {
			e := editBuf{buf: m.input.buf, cur: len(m.input.buf), lo: 0, hi: len(m.input.buf), written: m.input.written}
			e.cur = 0
			edits.kill(&e, len(m.input.buf), "")
			edits.last = ""
			m.input.Reset()
			m.refresh()
			return msg, nil, true
		}
	case "ctrl+l":
		m.refresh()
		return msg, tea.ClearScreen, true
	case "ctrl+p", "ctrl+n":
		if m.focus == focusInput {
			code := tea.KeyUp
			if key == "ctrl+n" {
				code = tea.KeyDown
			}
			return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}, nil, false
		}
	case "alt+<", "alt+>":
		if m.focus == focusInput {
			if key == "alt+<" {
				m.input.cursor = 0
				m.input.clampCursor()
			} else {
				m.input.CursorEnd()
			}
			m.refresh()
			return msg, nil, true
		}
	}
	return msg, nil, false
}

// boundAction is the chord action key is bound to, when someone gave one a
// single key in keys.json.
func (m *tuiModel) boundAction(key string) string {
	for _, a := range chordActions {
		if m.keys.is(a, key) {
			return a
		}
	}
	return ""
}

// runChord does what a finished chord is bound to; an unbound one says so.
func (m *tuiModel) runChord(chord string) tea.Cmd {
	if a := m.boundAction(chord); a != "" {
		return m.runAction(a)
	}
	m.append(StDim.Render(chord + " does nothing (/keys lists the keys)"))
	return nil
}

func (m *tuiModel) runAction(action string) tea.Cmd {
	switch action {
	case "thinking.fold":
		m.toggleThinking()
	case "paste.text":
		if m.focus == focusInput {
			return pasteTextCmd()
		}
	case "edit.external":
		if m.focus == focusInput {
			return m.editExternal()
		}
	}
	return nil
}

// editorDoneMsg is the editor's exit: the file's text, or why there is none.
type editorDoneMsg struct {
	text string
	err  error
}

// editExternal opens the message in $VISUAL, else $EDITOR, else vi, as a
// shell's ctrl+x ctrl+e does; saving and quitting puts the text back.
func (m *tuiModel) editExternal() tea.Cmd {
	dir, err := os.MkdirTemp("", "dun-edit-")
	if err != nil {
		m.append(StErr.Render("edit: " + err.Error()))
		return nil
	}
	path := filepath.Join(dir, "message.md")
	if err := os.WriteFile(path, []byte(m.input.Value()), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		m.append(StErr.Render("edit: " + err.Error()))
		return nil
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// An editor may carry arguments ("code --wait"); the file goes last.
	f := strings.Fields(editor)
	cmd := exec.Command(f[0], append(f[1:], path)...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.RemoveAll(dir)
		if err != nil {
			return editorDoneMsg{err: err}
		}
		b, err := os.ReadFile(path)
		return editorDoneMsg{text: strings.TrimRight(string(b), "\n"), err: err}
	})
}

// editorDone puts the edited text in the composer; an editor that failed
// leaves the message as it was.
func (m *tuiModel) editorDone(msg editorDoneMsg) {
	if msg.err != nil {
		m.append(StErr.Render("edit: " + msg.err.Error() + " — the message is unchanged"))
		return
	}
	before := editSnap{buf: m.input.buf, cur: m.input.cursor}
	m.input.SetValue(msg.text)
	m.input.CursorEnd()
	edits.pushUndo(composerID, before) // ctrl+_ takes the edit back
	m.refresh()
}
