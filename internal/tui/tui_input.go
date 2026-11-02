package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/proto"
)

func (m tuiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.traceResize(msg)
		first, prevW := m.w == 0 && m.h == 0, m.w
		m.w, m.h = msg.Width, msg.Height
		// Layout: head + top + convo + divider(1) + lower + status(1).
		// convoHeight() is the one place that budget lives — see it for why the
		// viewport's own height has to match the drawn one.
		//
		// Resize the viewport in place rather than building a new one: a fresh
		// viewport starts at YOffset 0, so every resize threw the reader back to
		// the top of the conversation. On a phone the soft keyboard opening and
		// closing is a resize, and it fires constantly.
		if first {
			m.vp = newConvoPane(msg.Width, msg.Height-4)
		} else {
			m.vp.Width = max(1, msg.Width)
		}
		m.input.width = msg.Width - 2
		m.search.SetWidth(msg.Width - 4)
		// Only when the WIDTH actually changed: a renderer is word-wrap state
		// and nothing else, and rebuilding one is not free. A height-only
		// resize (the common one — an ask panel growing) rebuilt it for nothing.
		if m.md == nil || m.mdWidth != msg.Width {
			// Prose wraps at the question band's edge (bandCap): a 170-column
			// line on a wide screen is hard to follow back to its start.
			m.md, m.mdWidth = newMarkdown(min(msg.Width, bandCap)-2), msg.Width
		}
		if m.inspecting {
			m.insp.setSize(m.w, m.h)
		}
		// A height-only resize does not change a single wrapped line — only the
		// row budget moved. Re-wrapping and re-joining the whole scrollback for
		// that is pure waste, and on a phone the soft keyboard makes it the most
		// frequent message there is. The funnel in Update re-heights the viewport
		// and re-runs the scroll policy, which is the entire job here.
		if !first && msg.Width == prevW {
			return m, nil
		}
		// Now that the new width has reached the input (lowerView measures it),
		// the row budget is knowable — refresh() scrolls against it.
		m.vp.setHeight(m.convoHeight())
		m.refresh()
		return m, nil

	case loginProbeMsg:
		if m.loginPicking {
			m.loginProbe[msg.id] = msg.n
			m.refresh()
		}
		return m, nil

	case modelFetchMsg:
		if m.modelPicking && !m.providerForm && !m.loginPicking {
			m.addModelRows(msg)
			m.refresh()
		}
		return m, nil

	case loginCheckedMsg:
		return m, m.loginChecked(msg)

	case ragLoadedMsg:
		m.ragLoaded(msg)
		return m, nil
	case ragModelsMsg:
		m.ragModels(msg)
		return m, nil
	case ragSavedMsg:
		m.ragSaved(msg)
		return m, nil

	case tea.KeyPressMsg:
		// Any key clears a mouse highlight; esc does only that.
		if m.drag.on {
			shown := m.drag.shown()
			m.drag = mouseSel{}
			if shown && msg.String() == "esc" {
				return m, nil
			}
		}
		// ctrl+g while a turn runs interrupts it, whatever has the keys. Idle it
		// does nothing; ctrl+k stays the input's kill-to-end-of-line, busy or not.
		if m.keys.is("turn.interrupt", msg.String()) && m.turnRunning() {
			return m.interrupt()
		}
		// ctrl+c while a turn runs interrupts it too; a second ctrl+c within
		// quitAgain falls through to the quit below (USER 2026-10-22).
		if msg.String() == "ctrl+c" && m.turnRunning() && time.Since(m.ctrlCAt) > quitAgain {
			m.ctrlCAt = time.Now()
			return m.interrupt()
		}
		// A y/n question (kill job N?) takes the next key, whatever it is.
		if m.confirm.q != "" {
			return m.updateConfirm(msg)
		}
		// The tool inspector overlay owns every key while open.
		if m.inspecting {
			open, cmd := m.insp.update(msg)
			m.inspecting = open
			return m, cmd
		}
		// So does a job's view, all but ctrl+c (jobview.go).
		if m.jobViewing && msg.String() != "ctrl+c" {
			open, cmd := m.jv.key(msg)
			if !open {
				m.jobViewing = false
				m.refresh()
			}
			return m, cmd
		}
		// A pushed screen (panes.go) has every key but ctrl+c: tab moves
		// between its panes, the focused one takes the rest.
		if s := m.activeScreen(); s != nil && msg.String() != "ctrl+c" {
			s.key(&m, msg)
			if m.paging {
				m.syncPager()
			}
			return m, nil
		}
		m.scr = nil
		// So does the pager, all but ctrl+c: it reads one block in the convo
		// pane's place, and esc brings the conversation back.
		if m.paging && msg.String() != "ctrl+c" {
			if v := m.toolsPaging(); v != nil && (msg.String() == "space" || m.toolKey(v, msg.String()) || m.filterKey(v, msg.String(), msg.Text)) {
				if msg.String() == "space" {
					m.toggleMCPTool(v)
				}
				return m, nil
			}
			if v := m.switchesPaging(); v != nil && m.switchKey(v, msg.String(), msg.Text) {
				return m, nil
			}
			if v := m.serversPaging(); v != nil && m.serverKey(v, msg.String()) {
				m.refresh()
				return m, nil
			}
			// "i" on a call opens the inspector over the pager; esc there comes
			// back here.
			if n := m.pg.curNode(); msg.String() == "i" && m.pg.tree() && n != nil && n.insp != nil {
				m.insp = newInspector(n.insp.name, n.insp.input, n.insp.output)
				m.insp.setSize(m.w, m.h)
				m.inspecting = true
				return m, nil
			}
			if !m.pg.key(msg.String()) {
				m.paging = false
				m.refresh()
			}
			return m, nil
		}
		// Answering an ask_user is a mode of its own (select an option, add a
		// detail, or type a custom/chat answer) — it owns the keys.
		if m.asking {
			return m.updateAsking(msg)
		}
		// So does the session picker.
		if m.picking {
			return m.updatePicking(msg)
		}
		// So does the add-MCP-server form.
		if m.mcpForm {
			return m.updateMCPForm(msg)
		}
		// So does /rag setup.
		if m.rag != nil {
			return m.updateRagSetup(msg)
		}
		// So does the model picker.
		if m.modelPicking {
			return m.updateModelPicking(msg)
		}
		// Typing a "/" search query owns the keys until enter/esc.
		if m.searching {
			return m.updateSearch(msg)
		}
		// The activity zone owns its keys while focused: ↑/↓ select, → descends,
		// ← ascends and finally leaves. Anything it does not claim (tab, ctrl+c,
		// "/") falls through to the switch below.
		if m.focus == focusActivity && !m.asking && !m.searching {
			if cmd, ok := m.actKeys(msg.String()); ok {
				m.refresh()
				return m, cmd
			}
		}
		// Descended into a box, the keys scroll it (box.go); into an answer's
		// wide blocks, they scroll them sideways (wide.go).
		if m.insideKeys(msg.String()) || m.insideWideKeys(msg.String()) {
			m.refresh()
			return m, nil
		}
		// y yanks the selected block's text to the clipboard (vim's yank).
		if msg.String() == "y" && m.focus == focusConvo && m.sel >= 0 && m.sel < len(m.convo) {
			if text := yankText(m.convo[m.sel]); text != "" {
				return m, copyCmd(text)
			}
			return m, nil
		}
		// l descends into the selected box, like →.
		if msg.String() == "l" && m.selBox() != nil {
			m.selBox().inside = true
			m.refresh()
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			if m.disableExit {
				return m, nil // exit disabled — use /exit
			}
			m.quitting = true // deliberate exit: do not respawn the engine
			return m, tea.Quit
		case "esc":
			if m.searchActive { // leave match-scroll mode, back to free selection
				m.searchActive = false
				m.matches = nil
				m.refresh()
				return m, nil
			}
			if d := m.selDocs(); d != nil && d.descended { // ascend out of the doc list
				d.descended = false
				m.refresh()
				return m, nil
			}
			if m.paletteActive() { // dismiss the command palette, keep the app
				m.input.Reset()
				m.paletteSel = 0
				m.refresh()
				return m, nil
			}
			// The conversation's selection is a level too: esc drops it and
			// gives the keys back to the input (USER 2026-10-22 — the backout
			// after a click or ←). Inner levels (a box, docs, search) closed
			// above.
			if m.focus == focusConvo && m.insideBox() == nil {
				m.setFocus(focusInput)
				m.sel = -1
				m.refresh()
				return m, textinput.Blink
			}
			// Agent scope is the outermost thing esc can be inside. Quitting dun
			// from within a child's conversation is not "one level out" by any
			// reading, and it was what esc did.
			if m.scopeAgent != 0 {
				return m, m.leaveAgentScope()
			}
			// Nothing else wanted it, and a turn runs: esc stops the turn.
			if m.turnRunning() {
				return m.interrupt()
			}
			// esc is "one level out", and there is no level out of dun: it
			// says how to leave instead of leaving.
			return m, m.exitFlash()
		case "/":
			if m.focus == focusConvo {
				m.searching, m.searchActive, m.matches = true, false, nil
				m.search.Reset()
				m.search.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
		case "tab":
			// In the command palette, tab completes to the highlighted command.
			if m.paletteActive() {
				if cmd, l, ok := m.paletteArgs(); ok {
					if len(l.choices) > 0 {
						sel := min(max(m.paletteSel, 0), len(l.choices)-1)
						m.input.SetValue("/" + cmd + " " + l.choices[sel].value + " ")
						m.input.CursorEnd()
						m.paletteSel = 0
						m.refresh()
					}
					return m, nil
				}
				if ms := m.paletteMatches(); len(ms) > 0 {
					sel := m.paletteSel
					if sel < 0 || sel >= len(ms) {
						sel = 0
					}
					m.input.SetValue("/" + ms[sel].Name + " ")
					m.input.CursorEnd()
					m.paletteSel = 0
					m.refresh()
				}
				return m, nil
			}
			m.cycleFocus(1)
			m.refresh()
			return m, textinput.Blink
		case "shift+tab":
			m.cycleFocus(-1)
			m.refresh()
			return m, textinput.Blink
		case "pgup", "pgdown":
			if msg.String() == "pgdown" && m.grantBaseline(max(1, m.vp.Height-1)) {
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.updateScrollPin()
			return m, cmd
		case "ctrl+end": // end a scroll-back: newest output, following again
			m.scrollToEnd()
			return m, nil
		case "ctrl+t": // every thought: its one line, or in full
			m.toggleThinking()
			return m, nil
		case "ctrl+home": // top of the last user message: the current exchange
			m.scrollToLastUser()
			return m, nil
		case "enter":
			if m.focus == focusConvo {
				// Inside a doc list: enter expands/collapses the current document.
				if d := m.selDocs(); d != nil && d.descended {
					if d.cur >= 0 && d.cur < len(d.docs) {
						d.docs[d.cur].open = !d.docs[d.cur].open
						m.refresh()
					}
					return m, nil
				}
				// The open question, put away earlier, comes back.
				if m.askParked && m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].askOpen {
					return m.reopenAsk()
				}
				// The "not logged in" line (or the kept question's) does
				// what it says.
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].needLogin && !m.loggedIn() {
					m.setFocus(focusInput)
					return m, m.openLogin("")
				}
				// Thinking (and anything else with a page) opens in the pager.
				if m.openPager(m.sel) {
					return m, nil
				}
				// Otherwise cycle the focused block through its view states.
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].expandable() {
					m.convo[m.sel].state = m.convo[m.sel].state.Next()
					if m.convo[m.sel].state == viewMinimized && m.convo[m.sel].docs != nil {
						m.convo[m.sel].docs.descended = false // closing collapses the descent
					}
					m.refresh()
				}
				return m, nil
			}
			if m.suggestActive() && m.suggestSel < len(m.suggestions) { // accept the highlighted suggestion into the input
				m.input.SetValue(m.suggestions[m.suggestSel].text)
				m.input.CursorEnd()
				m.suggestions = nil // hide; they reappear when the input is cleared
				m.refresh()
				return m, nil
			}
			// Enter submits the message. Alt+Enter inserts a newline in the
			// multiline buffer (so you can compose multi-line messages).
			if msg.String() == "alt+enter" {
				// Alt+Enter → insert newline.
				m.input = m.input.HandleKey(msg)
				m.refresh()
				return m, nil
			}
			v := strings.TrimSpace(m.input.Value())
			if strings.HasPrefix(v, "/") {
				return m, m.runPaletteEnter(v)
			}
			// Bare "exit" (no slash) is a hidden command: exits silently.
			if v == "exit" {
				return m, m.exit()
			}
			// Sending while a turn runs is allowed: the engine buffers the message
			// and lifts it into the next tool result, so the agent reads it inside
			// the turn it is already running. Before `ready` there is no engine
			// to receive it: it is kept, and sent at ready (startqueue.go).
			if v == "" {
				// Not logged in, enter on an empty composer is the login
				// the screen asks for (George, run 5: "make Enter open the
				// login form").
				if !m.loggedIn() && !m.starting && len(m.input.Images()) == 0 {
					return m, m.openLogin("")
				}
				return m, nil
			}
			if m.starting && m.scopeAgent == 0 {
				return m.keepUntilReady(v, m.input.Images()), nil
			}
			return m.sendUserImages(v, m.input.Images()), nil
		case "ctrl+up":
			// Ctrl+Up: navigate history (previous entry). The only way to reach
			// history — plain ↑ scrolls the conversation (see the ↑ case).
			if m.focus == focusConvo {
				// In convo focus, Ctrl+Up is ignored (plain Up handles convo nav).
				return m, nil
			}
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
				m.refresh()
			}
			return m, nil
		case "ctrl+down":
			// Ctrl+Down: navigate history (next entry).
			if m.focus == focusConvo {
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
				m.refresh()
			}
			return m, nil
		case "up":
			if m.focus == focusConvo {
				if d := m.selDocs(); d != nil && d.descended { // move within the doc list
					if d.cur > 0 {
						d.cur--
					}
					m.refresh()
					return m, nil
				}
				if m.searchActive && len(m.matches) > 0 { // step to the previous match
					if m.matchPos > 0 {
						m.matchPos--
					}
					m.sel = m.matches[m.matchPos]
				} else if top, h := m.selGeom(); m.vp.Height > 0 && h > m.vp.Height && top < m.vp.YOffset {
					m.vp.SetYOffset(m.vp.YOffset - 1) // scroll up within a tall message first
					return m, nil
				} else if m.sel > 0 {
					m.sel = m.selSkipUp(m.sel - 1)
				}
				m.refresh()
				return m, nil
			}
			// One rule everywhere on this axis: if ↑ has somewhere to go INSIDE
			// the thing you are in, it goes there; at the edge it falls through
			// and scrolls the conversation. So the picker and the palette move
			// their selection until it reaches the top and then stop capturing
			// the key — reading the options is exactly when you want to scroll
			// back through what was said.
			if m.suggestActive() && m.suggestSel > 0 {
				m.suggestSel--
				m.refresh()
				return m, nil
			}
			if m.paletteActive() && m.paletteSel > 0 {
				m.paletteSel--
				m.refresh()
				return m, nil
			}
			// ↑ with messages queued for the running turn takes them back into
			// the composer, merged, to fix and send again (USER 2026-10-27).
			// Once: the queue is then empty, and the next ↑ is history or the
			// scroll, as before.
			if m.queuedMsgs > 0 && m.input.AtFirstLine() && !m.paletteActive() {
				if m.proc.controlCmd("unqueue", "") {
					m.queuedMsgs = 0
				}
				return m, nil
			}
			// ↑ on an EMPTY composer recalls the last thing sent, and on a
			// recalled one the one before — the shell's ↑ (USER 2026-10-23).
			// A composer with text of the person's own keeps the rule below.
			if m.histBrowsing() && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
				m.refresh()
				return m, nil
			}
			// Plain up moves within the composer until it runs out of buffer;
			// from the FIRST row it scrolls the CONVERSATION. History lives on
			// ctrl+↑/↓ instead, because of Termux: with mouse tracking inactive
			// on the alt screen — the state that lets a tap raise the soft
			// keyboard, see the mouse note in runTUI and TerminalView.doScroll —
			// a finger-scroll arrives as a RUN of plain ↑/↓ keys. Walking
			// history on those made every swipe stomp the composer; one viewport
			// row per key is exactly what the terminal is describing.
			if m.input.AtFirstLine() {
				m.vp.SetYOffset(m.vp.YOffset - 1)
				m.updateScrollPin()
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			m.refresh()
			return m, nil
		case "down":
			if m.focus == focusConvo {
				if d := m.selDocs(); d != nil && d.descended { // move within the doc list
					if d.cur < len(d.docs)-1 {
						d.cur++
					}
					m.refresh()
					return m, nil
				}
				if m.searchActive && len(m.matches) > 0 { // step to the next match
					if m.matchPos < len(m.matches)-1 {
						m.matchPos++
					}
					m.sel = m.matches[m.matchPos]
				} else if top, h := m.selGeom(); m.vp.Height > 0 && h > m.vp.Height && top+h > m.vp.YOffset+m.vp.Height {
					m.vp.SetYOffset(m.vp.YOffset + 1) // scroll down within a tall message first
					return m, nil
				} else if m.sel < len(m.convo)-1 {
					m.sel = m.selSkipDown(m.sel + 1)
				}
				m.refresh()
				return m, nil
			}
			// …and ↓ mirrors it: move within the picker/palette to the LAST
			// entry, then fall through and scroll (see the ↑ case).
			if m.suggestActive() && m.suggestSel < len(m.suggestions)-1 {
				m.suggestSel++
				m.refresh()
				return m, nil
			}
			if m.paletteActive() && m.paletteSel < m.paletteRows()-1 {
				m.paletteSel++
				m.refresh()
				return m, nil
			}
			// On a recalled entry, ↓ goes forward through history, past the
			// newest to an empty composer again.
			if m.histIdx < len(m.history) && m.histBrowsing() {
				m.histIdx++
				if m.histIdx == len(m.history) {
					m.input.SetValue("")
				} else {
					m.input.SetValue(m.history[m.histIdx])
					m.input.CursorEnd()
				}
				m.refresh()
				return m, nil
			}
			// …and from the LAST row, ↓ scrolls the conversation forward (see
			// the ↑ note above).
			if m.input.AtLastLine() {
				m.vp.SetYOffset(m.vp.YOffset + 1)
				m.updateScrollPin()
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			m.refresh()
			return m, nil
		case "right":
			// Inside the conversation → means DESCEND, uniformly. `▸` already
			// means "there is something inside this" on the activity tree and on
			// a docs list, and both are opened with →; a message wearing the same
			// glyph that could only be opened with enter was the odd one out.
			//
			// A block with nothing left to open stops →: arrows never change the
			// focus zone, only tab/shift+tab do (USER 2026-10-23 — "the movement
			// between panes is incredibly obnoxious"). → used to hand the keys
			// back to the input here, and ← at the input's front hopped to the
			// conversation, so an arrow held one press too long left the pane.
			if m.focus == focusConvo {
				if m.descendSel() {
					m.refresh()
				}
				return m, nil
			}
			// focusInput: right from an EMPTY input accepts the ghost-text suggestion
			// (fills the buffer so the user can edit before sending).
			if m.suggestActive() && m.suggestSel < len(m.suggestions) {
				m.input.SetValue(m.suggestions[m.suggestSel].text)
				m.input.CursorEnd()
				m.suggestions = nil // hide; reappear when input is cleared
				m.refresh()
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
		case "left":
			if m.focus == focusConvo {
				// ← is exactly → run backwards: close the innermost thing that is
				// open, one level per press — the descend rule first, always.
				if m.ascendSel() {
					m.refresh()
					return m, nil
				}
				// Out of levels inside this conversation: if the conversation is a
				// CHILD's, the next level out is the session itself. That is what
				// the status bar promises, and until now nothing delivered it.
				if m.scopeAgent != 0 {
					return m, m.leaveAgentScope()
				}
				// Nothing to ascend out of: the edge. ← stops here rather than
				// moving to another zone (USER 2026-10-23, see the → case).
				return m, nil
			}
			// focusInput: left at the FRONT of the input, in a child's scope,
			// goes back to the session — scope is where the user lands on
			// descending, so it is where they press ← to undo it. That is a
			// level of the conversation, not a zone: the keys stay in the input.
			// Outside a scope the front of the input is an edge and ← stops.
			if m.input.Position() == 0 {
				if m.scopeAgent != 0 {
					return m, m.leaveAgentScope()
				}
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
		default:
			if m.focus == focusConvo { // keys don't type into a blurred input
				return m, nil
			}
			// Suggestion quick-pick: a digit while the picker is showing fills
			// the input (not sends), so the user can edit before sending.
			if m.suggestActive() {
				if k := msg.String(); len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
					if n := int(k[0] - '0'); n <= len(m.suggestions) {
						m.input.SetValue(m.suggestions[n-1].text)
						m.input.CursorEnd()
						m.suggestions = nil // hide; reappear when input is cleared
						m.refresh()
						return m, nil
					}
				}
			}
			// ctrl+v: an image on the clipboard goes in as an image token.
			// Text is pasted by the terminal, as before.
			if msg.String() == "ctrl+v" {
				return m, pasteImageCmd()
			}
			// ctrl+p: the clipboard's text, read by dun (USER 2026-10-27: no
			// paste key beside ctrl+k and ctrl+u); a terminal's own paste is
			// a PasteMsg, as before.
			if msg.String() == "ctrl+p" {
				return m, pasteTextCmd()
			}
			m.input = m.input.HandleKey(msg)
			m.paletteSel = 0 // typing re-filters the palette; start at the top
			m.refresh()
			return m, nil
		}

	case tea.PasteMsg:
		return m.paste(msg)

	case versionLatestMsg:
		m.append(StDim.Render(msg.line))
		m.refresh()
		return m, nil

	case updateNoteMsg:
		// The release check's one line: a new build in place, or a refused one.
		if msg != "" {
			m.append(stNote.Render(string(msg)))
			m.refresh()
		}
		return m, nil

	case repaintNowMsg:
		return m, m.repaintNow(msg)

	case repaintDoneMsg:
		m.repaintDone(msg)
		return m, nil

	case dumpMsg:
		m.writeDump()
		return m, waitForDump(m.dumpSig) // re-arm for the next signal

	case procMsg:
		if msg.p != m.proc {
			// A connection this TUI has moved on from: none of its events are
			// this conversation's. Keep draining it, so its reader can finish,
			// until it ends.
			if _, end := msg.msg.(eofMsg); end {
				return m, nil
			}
			return m, waitEvent(msg.p)
		}
		return m.update(msg.msg)

	case evMsg:
		if msg["type"] == "self_reexec" && !m.replaying {
			// This binary on disk is the build dund runs now: quit and
			// re-exec into it on this session, as /reload does.
			m.reloadReq = true
			return m, tea.Quit
		}
		m.traceEvent(msg)
		nm := m.handleEvent(msg)
		cmds := []tea.Cmd{waitEvent(nm.proc)}
		if nm.renderDue && !nm.tickPending {
			nm.tickPending = true
			cmds = append(cmds, renderTick())
		}
		return nm, tea.Batch(cmds...)

	case idleSuggestMsg:
		m.idleTickPending = false
		// A keystroke since the tick was armed moves the deadline: wait out the
		// remainder rather than firing early. This is what makes it a debounce
		// and not a 3-second poll.
		if wait := idleSuggestDelay - time.Since(m.lastKeyAt); wait > 0 {
			m.idleTickPending = true
			return m, idleSuggestTick(wait)
		}
		if m.idleSuggestReady() {
			m.suggestedThisIdle = true
			m.proc.controlCmd("suggest", "") // best-effort: no engine, no suggestions
		}
		return m, nil

	case jobTailMsg:
		return m, m.jobTail(msg)

	case renderTickMsg:
		// One frame per tick at most, no matter how fast the tokens arrive.
		m.tickPending = false
		if m.renderDue {
			m.renderDue = false
			m.refresh()
			// Still streaming? Keep the clock running rather than waiting for
			// the next token to restart it — that would add a tick of latency
			// to every frame.
			if m.busy {
				m.tickPending = true
				return m, renderTick()
			}
		}
		return m, nil

	case eofMsg:
		// From an engine we already replaced: its death was the point.
		if msg.proc != nil && m.proc != nil && msg.proc != m.proc {
			return m, nil
		}
		// One this TUI closed itself (a session switch): the connection
		// ending was the point, even before its replacement is up.
		if msg.proc != nil && msg.proc.closed.Load() {
			return m, nil
		}
		return m.engineGone()

	case engineUpMsg:
		if msg.err != nil {
			// dund may be on its way back (a restart into a new build takes a
			// second; a crashed service is restarted by systemd after a few):
			// keep trying, more slowly, for reconnectFor.
			if m.reconnSince.IsZero() {
				m.reconnSince = time.Now()
			}
			// Nothing on screen yet (dund failed before the first attach): the
			// conversation being resumed is on disk — show it, read-only,
			// rather than an empty screen while this retries.
			if !m.everUp && !m.offlineShown {
				m.showSavedConversation()
			}
			m.reconnTries++
			if time.Since(m.reconnSince) >= reconnectFor {
				m.fatalErr = fmt.Sprintf("cannot reach the session service (dund) after %d attempts: %v", m.reconnTries, msg.err)
				m.append(StErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved: dun --continue"))
				m.reconnTries, m.reconnSince = 0, time.Time{}
				m.refresh()
				return m, nil
			}
			m.fatalErr = fmt.Sprintf("reconnecting… (attempt %d): %v", m.reconnTries+1, msg.err)
			if !m.everUp {
				m.fatalErr = fmt.Sprintf("starting session… (attempt %d): %v", m.reconnTries+1, msg.err)
			}
			m.refresh()
			return m, tea.Tick(reconnectDelay(m.reconnTries), func(time.Time) tea.Msg { return retryEngineMsg{} })
		}
		m.proc = msg.proc
		m.upgrade = nil // the dund reached says again if one is pending
		m.fatalErr = ""
		m.reconnTries, m.reconnSince = 0, time.Time{}
		// A reattach may replay from the middle of a session whose engine is
		// long up: no ready is coming. An engine that is starting says so
		// (engine "started"), and that is what waits for ready.
		if !msg.reattach {
			m.starting = true
			m.startingStart = time.Now()
		}
		m.refresh()
		return m, waitEvent(m.proc)

	case retryEngineMsg:
		return m, restartEngine(m.opts, m.sessionID, m.everUp, m.seen())

	case tea.MouseMsg:
		if m.paging {
			m.pg.mouse(msg)
			return m, nil
		}
		if m.jobViewing {
			m.jv.mouse(msg)
			return m, nil
		}
		// Clicks and drags (mouse.go); everything below is the wheel.
		if _, wheel := msg.(tea.MouseWheelMsg); !wheel {
			return m.mouseLeft(msg)
		}
		// The wheel scrolls a box only once you are inside it.
		if b := m.insideBox(); b != nil {
			switch msg.Mouse().Button {
			case tea.MouseWheelUp:
				b.scroll(-wheelDelta)
			case tea.MouseWheelDown:
				b.scroll(wheelDelta)
			}
			m.refresh()
			return m, nil
		}
		if msg.Mouse().Button == tea.MouseWheelDown && m.grantBaseline(wheelDelta) {
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // wheel scrolls the conversation viewport
		m.updateScrollPin()
		return m, cmd

	case copiedMsg:
		return m, m.flash(copiedNote(msg))

	case upgradeSelfMsg:
		if msg.err != nil {
			m.append(StErr.Render("upgrade: " + msg.err.Error()))
		}
		return m, nil

	case clipTextMsg:
		if msg.err != nil {
			return m, m.flash(msg.err.Error())
		}
		return m.paste(tea.PasteMsg{Content: msg.text})

	case clipImageMsg:
		switch {
		case msg.err != nil:
			return m, m.flash(msg.err.Error())
		case msg.ok:
			m.input.InsertImage(msg.path, msg.name, msg.w, msg.h)
			m.refresh()
		}
		return m, nil

	case flashMsg:
		return m, nil // the redraw is the point: an expired note leaves the status line

	case pulseMsg:
		m.pulsePending = false
		if m.pulseWanted() {
			m.pulse++
			m.repaintBand()
		}
		return m, nil

	case blinkTickMsg:
		// Only refresh when content actually changed — a blind refresh on
		// every blink tick re-wraps the entire scrollback and pegs the CPU
		// for long conversations with tool calls.
		if m.renderDue {
			m.renderDue = false
			m.refresh()
		}
		return m, m.input.BlinkTick()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		// /mcp's rows still starting turn with it.
		if v := m.serversPaging(); v != nil && v.starting() {
			v.spin = m.spin.View()
			m.syncPager()
		}
		if v := m.extPaging(); v != nil && v.starting() {
			v.spin = m.spin.View()
			m.syncPager()
		}
		// A server still starting: the live splash counts its seconds. The
		// row changes in place; refresh only when its words did.
		if i := m.liveSplashAt(); len(m.srvSince)+len(m.idxSince) > 0 && i >= 0 &&
			m.splashView(m.splashW()) != m.convo[i].collapsed {
			m.refresh()
		}
		return m, cmd
	}
	return m, nil
}

// updateAsking drives the answer picker: ↑/↓ highlight an option (the last row
// is a custom/chat free-text answer), enter selects, `n` attaches a detail to
// the highlighted option, tab or → opens the option as a custom answer to edit.
// While typing a detail or a custom answer, keys go to the input; esc backs
// out of that sub-mode.
func (m tuiModel) updateAsking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	custom := len(m.askOptions) // index of the trailing "custom / chat" row
	// In multi mode a "✓ done" row follows the custom row (submits the checked
	// set) — so options toggle on enter without stealing space (a typed char).
	maxRow := custom
	if m.askMulti {
		maxRow = custom + 1
	}
	switch msg.String() {
	case "ctrl+c":
		if m.disableExit {
			return m, nil
		}
		m.quitting = true // deliberate exit: do not respawn the engine
		return m, tea.Quit
	case "esc":
		// One level out: from a detail or a custom answer back to the options,
		// and from the options out of the question — which stays open.
		if m.noting || (m.customAnswer && len(m.askOptions) > 0) {
			m.noting, m.customAnswer = false, false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		}
		return m.parkAsk()
	case "enter":
		switch {
		case m.noting:
			m.askNote = strings.TrimSpace(m.input.Value())
			m.noting = false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		case m.customAnswer && m.askMulti:
			// Multi: the field edits an option or adds one; enter keeps it,
			// checked, and goes back to the list. Done sends the checked set.
			v := strings.TrimSpace(m.input.Value())
			if v != "" {
				if m.askEdit >= 0 && m.askEdit < len(m.askOptions) {
					m.askOptions[m.askEdit], m.askChecked[m.askEdit] = v, true
					m.askSel = m.askEdit
				} else {
					m.askOptions = append(m.askOptions, v)
					m.askChecked = append(m.askChecked, true)
					m.askSel = len(m.askOptions) - 1
				}
			}
			m.customAnswer = false
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
		case m.askMulti && m.askSel < custom: // toggle the highlighted option
			m.askChecked[m.askSel] = !m.askChecked[m.askSel]
			m.refresh()
			return m, nil
		case m.askSel == custom: // open free-text / chat entry
			m.customAnswer, m.askEdit = true, custom
			m.input.Reset()
			m.input.placeholder = "type your answer, or chat…"
			if m.askMulti {
				m.input.placeholder = "another option: enter adds it, checked"
			}
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		case m.askMulti: // the "✓ done" row: submit the checked set
			var picked []string
			for i, on := range m.askChecked {
				if on {
					picked = append(picked, m.askOptions[i])
				}
			}
			if len(picked) == 0 {
				return m, nil // nothing checked yet
			}
			return m.sendAnswer(strings.Join(picked, ", ")), nil
		default:
			return m.sendAnswer(m.askAnswerAt(m.askSel)), nil
		}
	case "ctrl+end":
		m.scrollToEnd()
		return m, nil
	case "ctrl+home":
		m.scrollToLastUser()
		return m, nil
	// ↑/↓ follow the same rule here as everywhere else: move within the thing
	// you are in — the option list, or the caret when typing a detail/custom
	// answer — and at its edge fall through and scroll the conversation. A
	// question you cannot read the backlog behind is a question you cannot
	// answer, and the options are exactly when you want to look back.
	case "up":
		if m.noting || m.customAnswer {
			if !m.input.AtFirstLine() {
				break // let the caret move (handled below)
			}
		} else if m.askSel > 0 {
			m.askSel--
			m.refresh()
			return m, nil
		}
		m.vp.SetYOffset(m.vp.YOffset - 1)
		m.updateScrollPin()
		return m, nil
	case "down":
		if m.noting || m.customAnswer {
			if !m.input.AtLastLine() {
				break // let the caret move (handled below)
			}
		} else if m.askSel < maxRow {
			m.askSel++
			m.refresh()
			return m, nil
		}
		m.vp.SetYOffset(m.vp.YOffset + 1)
		m.updateScrollPin()
		return m, nil
	case "tab", "right":
		// Edit the highlighted answer before sending it: the custom field,
		// holding what enter would send from here. Esc goes back to the
		// options; nothing is sent.
		if !m.noting && !m.customAnswer && m.askSel < custom {
			m.customAnswer, m.askEdit = true, m.askSel
			m.input.Reset()
			if m.askMulti {
				// Edit this option's own text; enter keeps it, checked.
				m.input.SetValue(m.askOptions[m.askSel])
				m.input.CursorEnd()
				m.input.placeholder = "this option, as you want it"
				m.input.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			m.input.SetValue(m.askAnswerAt(m.askSel))
			m.input.CursorEnd()
			m.input.placeholder = "type your answer, or chat…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		}
	case "n":
		if !m.noting && !m.customAnswer && !m.askMulti && m.askSel < custom {
			m.noting = true
			m.input.Reset()
			m.input.placeholder = "add a detail…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		}
	}
	// The custom row IS a text field, so typing on it answers — no enter first.
	// A no-options ask already drops straight into text entry
	// (TestTUI_AskNoOptionsFreeText); this makes the row that offers the
	// same thing behave the same way, and the key that opened the field is the
	// key that types into it. Enter still opens it empty, and every other row
	// keeps its bindings (`n` types an n HERE, but still starts a detail there).
	if !m.noting && !m.customAnswer && m.askSel == custom && typedText(msg) {
		m.customAnswer, m.askEdit = true, custom
		m.input.Reset()
		m.input.placeholder = "type your answer, or chat…"
		if m.askMulti {
			m.input.placeholder = "another option: enter adds it, checked"
		}
		m.input.Focus()
		m.input = m.input.HandleKey(msg)
		m.refresh()
		return m, textinput.Blink
	}
	if m.noting || m.customAnswer { // typing into the detail / custom field
		m.input = m.input.HandleKey(msg)
		return m, nil
	}
	return m, nil
}

// paste routes a bracketed paste to the text field that has the keys, the way
// a key is routed in update — but only ever as text: a paste is never a
// command, whatever it spells.
func (m tuiModel) paste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	m.drag = mouseSel{}
	var cmd tea.Cmd
	switch {
	case m.confirm.q != "", m.paging, m.picking:
		return m, nil
	case m.inspecting:
		if m.insp.searching {
			m.insp.search, cmd = m.insp.search.Update(msg)
		}
		return m, cmd
	case m.jobViewing:
		if m.jv.searching {
			m.jv.search, cmd = m.jv.search.Update(msg)
		}
		return m, cmd
	case m.asking:
		if !m.noting && !m.customAnswer {
			if m.askSel != len(m.askOptions) {
				return m, nil
			}
			// The custom row is a text field: pasting on it answers, as typing does.
			m.customAnswer = true
			m.input.Reset()
			m.input.placeholder = "type your answer, or chat…"
			m.input.Focus()
			cmd = textinput.Blink
		}
		m.input = m.input.HandlePaste(msg.Content)
		m.refresh()
		return m, cmd
	case m.mcpForm:
		return m.typeMCPField(msg)
	case m.rag != nil:
		return m.typeRagQuery(msg)
	case m.modelPicking && m.loginPicking:
		return m.typeLoginQuery(msg)
	case m.modelPicking && m.providerForm:
		return m.typeProviderField(msg)
	case m.modelPicking:
		return m.typeModelQuery(msg)
	case m.searching:
		return m.typeSearch(msg)
	case m.focus == focusConvo: // keys don't type into a blurred input
		return m, nil
	}
	m.input = m.input.HandlePaste(msg.Content)
	m.paletteSel = 0 // typing re-filters the palette; start at the top
	m.refresh()
	return m, nil
}

// askAnswerAt is the answer the option at row i stands for: the option and
// its detail, or in multi mode the checked options with this one among them.
func (m tuiModel) askAnswerAt(i int) string {
	if !m.askMulti {
		ans := m.askOptions[i]
		if m.askNote != "" {
			ans += " — " + m.askNote
		}
		return ans
	}
	var picked []string
	for j, opt := range m.askOptions {
		if j == i || (j < len(m.askChecked) && m.askChecked[j]) {
			picked = append(picked, opt)
		}
	}
	return strings.Join(picked, ", ")
}

// typedText reports whether a key is the user typing a character, as opposed to
// driving the UI. Alt- and ctrl-modified keys are commands, not text — except
// ctrl+tab, which is how a tab is typed (multiline_input.go).
func typedText(msg tea.KeyPressMsg) bool {
	if msg.String() == "ctrl+tab" {
		return true
	}
	return msg.Text != "" && !msg.Mod.Contains(tea.ModAlt) && !msg.Mod.Contains(tea.ModCtrl)
}

// normalizeKey folds the spellings of shift+enter into ctrl+j, the composer's
// "insert a newline" key, so it adds a spelling rather than a meaning.
//
// Terminals report shift+enter three ways. CSI 13;2u (kitty, foot, wezterm)
// and CSI 27;2;13~ (xterm modifyOtherKeys) decode as "shift+enter". The common
// one, SS3 M, is the keypad-enter code, which Bubble Tea names "enter" — left
// alone it would SEND the message. Bubble Tea never turns on application
// keypad mode, so a real keypad enter sends CR and arrives as plain enter.
func normalizeKey(msg tea.Msg) tea.Msg {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return msg
	}
	if (k.Code == tea.KeyKpEnter && k.Mod == 0) || (k.Code == tea.KeyEnter && k.Mod == tea.ModShift) {
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	}
	return msg
}

// sendUser ships a user message to the engine (from the input or a suggestion)
// and clears transient UI (suggestions, input). The message is rendered when
// the engine's `user` event comes back, as it is on every other client.
func (m tuiModel) sendUser(v string) tuiModel { return m.sendUserImages(v, nil) }

// sendUserImages is sendUser for a message with the composer's images.
func (m tuiModel) sendUserImages(v string, imgs []proto.Image) tuiModel {
	// In agent scope the input steers that CHILD instead of the session. Same
	// gesture the parent's jobs(agent:N, tell:) makes, same code path in the
	// engine — there is one definition of what telling a child means.
	if m.scopeAgent != 0 && len(imgs) > 0 {
		// A child's tell is text: sent without its images, the message would
		// claim images the child never got. The draft stays.
		m.append(StErr.Render("images cannot be sent to a sub-agent — not sent"))
		m.refresh()
		return m
	}
	if m.scopeAgent != 0 {
		// A child takes a tell between its turns only (subagent.go, tell):
		// sent now, it would be refused and the words gone. The draft stays.
		if a := m.agentByID(m.scopeAgent); a != nil && a.state == "running" {
			m.flash(fmt.Sprintf("agent #%d is working — tell it when its turn ends (the draft is kept)", m.scopeAgent))
			return m
		}
		m.history = append(m.history, v)
		m.histIdx = len(m.history)
		m.input.Reset()
		// Not echoed here: the child's transcript takes it, and its
		// agent_entry shows it, on every client watching.
		if !m.proc.agentCmd(m.scopeAgent, "tell", v) {
			m.append(StErr.Render("no engine right now — not sent"))
		}
		m.scrollPinned = true
		m.refresh()
		return m
	}
	m.history = append(m.history, v)
	m.histIdx = len(m.history)
	m.input.Reset()
	m.restoredMsg = ""
	m.suggestions = nil
	m.saveUI()
	if m.replaying {
		m.append(StDim.Render("replaying a trace — there is no engine to send to"))
		m.refresh()
		return m
	}
	// Nothing to send to yet: held, and put back once a model is picked.
	if !m.loggedIn() && len(imgs) == 0 {
		m.holdUnsent(v)
		return m
	}
	if !m.proc.send(v, imgs) {
		m.append(StErr.Render("no engine right now — not sent. /reconnect, then send it again"))
		m.refresh()
		return m
	}
	m.busy = true
	m.scrollPinned = true // user just sent a message — show it
	m.refresh()
	return m
}

// sendAnswer resolves the ask: ships the answer to the engine and resets to
// the input pane.
func (m tuiModel) sendAnswer(v string) tuiModel {
	if !m.proc.answer(v) {
		m.append(StErr.Render("no engine right now — answer not sent"))
	}
	m = m.clearAsk()
	m.refresh()
	return m
}

// clearAsk returns to the input pane with no question open. The answer is
// rendered from the engine's `answered` event.
func (m tuiModel) clearAsk() tuiModel {
	m.setFocus(focusInput)
	m.dropAsk()
	m.askOptions, m.askSel, m.askNote = nil, 0, ""
	m.askMulti, m.askChecked = false, nil
	return m
}
