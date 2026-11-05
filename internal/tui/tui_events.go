package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// noteServers records which switchable servers are up, so a restart can put
// them back. Only ones with a command (/rag, /lsp) — the rest are the config's
// business, and the new engine reads the same config.
func (m *tuiModel) noteServers(ev evMsg) {
	list, ok := ev["servers"].([]any)
	if !ok {
		return
	}
	// Every server's id, for /mcp's completion.
	m.serverIDs = m.serverIDs[:0]
	m.servers, _ = serverStates(list)
	m.noteServerOrder()
	m.noteStarting(list)
	for _, s := range list {
		if st, ok := s.(map[string]any); ok && str(st["id"]) != "" {
			m.serverIDs = append(m.serverIDs, str(st["id"]))
		}
	}
	m.noteServerView(withBuiltins(ev, list))
	m.noteExtServers()
	if m.liveSplashAt() >= 0 {
		m.refresh() // the splash's servers and extras rows say it now
	}
}

// handleEvent applies one engine event. Inside a child's scope the root's
// events still belong to the root's conversation: it is put back on the
// model for them and stashed again after, so nothing it receives is lost and
// none of it lands in the child's view. A question needs the screen, so it
// leaves the scope first.
func (m tuiModel) handleEvent(ev evMsg) tuiModel {
	if m.scopeAgent == 0 {
		return m.applyEvent(ev)
	}
	switch ev["type"] {
	case "agents", "jobs", "agent_history", "agent_entry", "agent_told":
		return m.applyEvent(ev)
	case "ask", "mount":
		m.leaveAgentScope()
		return m.applyEvent(ev)
	}
	child := m.takeView()
	m.putView(m.rootView)
	m = m.applyEvent(ev)
	if m.scopeAgent == 0 { // the event ended the scope (a reset, an exit)
		return m
	}
	m.rootView = m.takeView()
	m.putView(child)
	if ev["type"] == "ready" {
		// A restarted engine: the watch went with the old one.
		m.proc.agentCmd(m.scopeAgent, "view", "")
	}
	return m
}
func (m tuiModel) applyEvent(ev evMsg) tuiModel {
	switch ev["type"] {
	case "workspace":
		// An empty branch is /isolate off: back in place, no worktree; the
		// control reply already said so.
		m.branch = str(ev["branch"])
		if m.branch != "" {
			m.append(StDim.Render("worktree branch: " + m.branch))
		}
	case "session":
		// Remembered so a respawned engine reattaches to THIS conversation
		// rather than starting a new one. Before this point there is nothing to
		// reattach TO, and guessing (--continue) would grab an unrelated session.
		m.sessionID = str(ev["id"])
		m.everUp = true
		if !m.splashDone && evNum(ev["resumed"]) == 0 {
			m.splashWant = true
		}
		m.proc.uiGet() // this user's UI state in the session, kept by dund
	case "ready":
		m.starting = false
		if m.keysErr != nil { // said once, where the person is looking
			m.append(warnLine("keys: " + m.keysErr.Error() + " — using the defaults (/keys)"))
			m.keysErr = nil
		}
		m.tools = nil
		if ts, ok := ev["tools"].([]any); ok {
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		// No "ready" line: the status bar says ready and the header counts
		// the tools. A session with nothing to send to says so when its
		// settings arrive (the settings case).
		// Show per-server start times for anything that took >1s.
		if srvs, ok := ev["servers"].([]any); ok {
			for _, sv := range srvs {
				s, ok := sv.(map[string]any)
				if !ok {
					continue
				}
				secs, _ := s["startSeconds"].(float64)
				if secs > 1.0 {
					m.append(StDim.Render(fmt.Sprintf("%s started in %ds", s["id"], int(secs+0.5))))
				}
			}
		}
		// What did not start, and a docs index's ingest summary, once per
		// TUI: a restart (a model pick) does not repeat it. Which optional
		// extras are off is in the splash and /status, not a line here.
		if hint := strings.TrimSpace(str(ev["hint"])); hint != "" && !m.hintShown {
			m.append(StDim.Render(hint))
			m.hintShown = true
		}
		m.afterSwitch()
		// No servers are turned back on here: a respawned engine reattaches
		// to this session, whose switches it keeps itself (dun mcpscope.go).
		m.noteServers(ev)
		return m
	case "server":
		list, _ := ev["servers"].([]any)
		opened := str(ev["id"]) == enginecmd.AllServers && m.openServerTable(withBuiltins(ev, list))
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" && !opened && !m.splashSays(ev) {
			m.noteServerMsg(str(ev["action"]), msg)
			if m.serversPaging() != nil {
				m.flashText, m.flashUntil = textfmt.OneLine(msg), time.Now().Add(flashFor)
			}
		}
		// The tool list changed; keep /help's count and the status bar honest.
		if ts, ok := ev["tools"].([]any); ok {
			m.tools = nil
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.noteServers(ev)
	case "catalog":
		m.noteCatalog(ev)
		m.refresh() // the splash's skills row
	case "mcp_tools":
		m = m.noteMCPTools(ev)
	case "switches":
		m = m.noteSwitches(ev)
	case "skill_file":
		m = m.noteSkillFile(ev)
	case "request":
		m.reqs.noteRequest(ev)
	case "extensions":
		m = m.noteExtensions(ev)
	case "plan":
		m = m.notePlan(ev)
	case "mcp_login":
		// Whole, not oneLine'd or clipped: the URL is to be copied.
		m.append(stNote.Render("🔑 " + enginecmd.LoginLine(str(ev["server"]), str(ev["url"]))))
	case "prompt":
		// An MCP prompt the user ran as a slash command, rendered by the
		// engine: it goes in as their message.
		if text := strings.TrimSpace(str(ev["text"])); text != "" {
			m = m.sendUser(text)
		}
	case "recap":
		// One dim line that OPENS. The scrollback keeps whatever was already
		// drawn — this is about the model's context, not about rewriting what you
		// have read — but a recap is no longer confirmed before it applies, so
		// the line has to be able to show what replaced the span. → expands it.
		note := StDim.Render("✂ " + str(ev["note"]))
		if detail := strings.TrimRight(str(ev["detail"]), "\n"); detail != "" {
			m.convo = append(m.convo, convoEntry{
				collapsed: StDim.Render("▸ ") + note,
				full:      StDim.Render("▾ ") + note + "\n" + StDim.Render(detail),
			})
			m.refresh()
		} else {
			m.append(note)
		}
		// Track for /context
		m.ctxStats.recaps++
		if entries := evNum(ev["entries"]); entries > 0 {
			m.ctxStats.entriesRecapped += int(entries)
		}
		if chars := evNum(ev["chars"]); chars > 0 {
			m.ctxStats.charsRecapped += int(chars)
		}
	case "control":
		// The queued messages taken back: out of the conversation's
		// "(queued)" lines and into the composer, ahead of what is there.
		if str(ev["id"]) == "unqueue" {
			var texts []string
			_ = json.Unmarshal([]byte(str(ev["action"])), &texts)
			if len(texts) == 0 {
				m.flashText, m.flashUntil = "nothing to take back: the turn already took them", time.Now().Add(flashFor)
				break
			}
			for _, t := range texts {
				for i := len(m.convo) - 1; i >= 0; i-- {
					if m.convo[i].provisional && m.convo[i].provisionalText == t {
						m.convo = append(m.convo[:i], m.convo[i+1:]...)
						break
					}
				}
			}
			merged := strings.Join(texts, "\n")
			if cur := m.input.Value(); strings.TrimSpace(cur) != "" {
				merged += "\n" + cur
			}
			m.input.SetValue(merged)
			m.input.CursorEnd()
			m.setFocus(focusInput)
			m.refresh()
			break
		}
		// /schedule edit <id>: the entry's text, into the composer to be
		// changed — in the client that asked; the others show the line.
		if str(ev["id"]) == "schedule-edit" {
			want := m.schedEditAsk
			m.schedEditAsk = ""
			if f := strings.Fields(str(ev["action"])); want != "" && len(f) >= 3 && f[2] == want {
				m.input.SetValue(str(ev["action"]))
				m.input.CursorEnd()
				m.setFocus(focusInput)
				m.refresh()
				break
			}
		}
		// /repaint report's answer: the TUI finishes it (repaintreport.go).
		if str(ev["id"]) == "repaint" {
			m.reportEvent(str(ev["action"]), str(ev["message"]))
			break
		}
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" {
			m.append(replyLine(msg))
		}
		if str(ev["id"]) == "interrupt" { // there was no turn to interrupt
			m.interrupting = false
		}
		// /close is terminal: the session it was editing no longer exists, so
		// staying attached to it would be showing a conversation that cannot be
		// resumed. Quit only once the engine has confirmed the work is done.
		if str(ev["id"]) == "close" {
			m.quitting = true
			m.proc.forget() // and the service lets the session go
		}
	case "reasoning":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.addThinking(str(ev["text"]))
	case "token":
		m.busy = true // a turn is active (incl. autonomous background-completion turns)
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.endRun() // the reply began: the run takes nothing more
		if m.liveThinking() >= 0 {
			// The reply has begun, so the round's thinking is over. Folding
			// changes the block's height, so this frame is drawn now, not on
			// the tick.
			m.endThinking()
			m.refresh()
		}
		m.cur += str(ev["text"])
		// Do NOT render here. A provider streaming 60 tokens/s would drive 60
		// full frames a second through the same Update loop that reads the
		// keyboard, and a frame is not free even with the wrap cache. Mark the
		// screen dirty; a tick renders at most renderHz. See renderTickMsg.
		m.renderDue = true
	case "reused":
		// A plain dun resumed an idle session (attach.go): say which.
		m.append(StDim.Render(str(ev["text"])))
	case "suggestions":
		// Not before anything was asked: there is nothing to predict from,
		// and a session that was just cleared must not show the old ones.
		if !m.asked() {
			break
		}
		m.suggestions = parseSuggestionItems(ev["items"])
		m.suggestSel = 0
		m.refresh()
	case "tool_call":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.endThinking()
		m.flushCur()
		args, _ := ev["args"].(map[string]any)
		if a := carriedMsg(str(ev["tool"]), args); a != nil {
			// A message to another agent: its own box (agentmsg.go).
			m.pendingTool = m.addMsg(a)
		} else {
			// Into the run's box (toolrun.go): appended, nothing above it moves.
			m.pendingTool = m.runAddCall(str(ev["tool"]), args)
		}
		m.activeTool = callText(str(ev["tool"]), args, 40)
		m.refresh()
	case "tool_result":
		result := str(ev["result"])
		idx := m.pendingTool
		if idx >= 0 && idx < len(m.convo) && m.convo[idx].msg != nil && !m.convo[idx].msg.done {
			m.convo[idx].msg.setResult(result)
			m.touchMsg(idx)
		} else {
			if idx < 0 || idx >= len(m.convo) || m.convo[idx].run == nil {
				idx = m.runAddCall(str(ev["tool"]), nil) // a result with no call seen
			}
			m.runSetResult(idx, str(ev["tool"]), result)
		}
		m.pendingTool = -1
		m.activeTool = ""
		// Track tool result stats for /context
		m.ctxStats.toolResults++
		if strings.Contains(result, "[") && strings.Contains(result, "characters elided") {
			m.ctxStats.resultsTruncated++
		}
		// liftQueued drains the buffer into every tool result. So any provisional
		// messages were just delivered to the LLM — clear the provisional marker
		// and restore normal style.
		for i := range m.convo {
			if m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		// Never re-pinned here: a delivered message was in view when it was
		// sent (sending pins), and a reader who has scrolled up since stays
		// where they are (USER 2026-10-28: zero bounce).
		m.queuedMsgs = 0
		m.refresh()
	case "history":
		if m.skipHistory {
			// A respawned engine replays the conversation it just resumed. It is
			// already on screen — rendering it again would double the scrollback.
			m.skipHistory = false
			break
		}
		items, _ := ev["items"].([]any)
		if len(items) > 0 && !m.splashKeep {
			m.splashWant = false // a conversation to show, not a fresh one
		}
		m.replay(items)
	case "message":
		// tokens already streamed the reply; nothing to add.
	case "agents":
		m.agents = agentRowsFromEvent(ev)
	case "job_usage":
		// What an open job view asked for (jobview.go): dund's sample of the
		// job's processes.
		if b, err := json.Marshal(ev); err == nil && m.jobViewing {
			var u proto.JobUsage
			if json.Unmarshal(b, &u) == nil && u.Job == m.jv.id {
				m.jv.usage = &u
				m.syncJobView()
			}
		}
	case "jobs":
		m.jobs = jobRowsFromEvent(ev)
	case "agent_history":
		// A child's scrollback, pulled on demand. Ignored unless it is the
		// child currently on screen: a late reply for a scope we have already
		// left would paste someone else's conversation over this one.
		if num(ev["agent"]) == m.scopeAgent && m.scopeAgent != 0 {
			m.convo, m.cur, m.pendingTool = nil, "", -1
			m.dropHold()
			if p := str(ev["prompt"]); p != "" {
				m.task = p
			}
			items, _ := ev["items"].([]any)
			// Not replay: no resume marker, and the last run stays open, since
			// a child at work gets its call's result as the next agent_entry.
			m.replayItems(items, -1)
			m.refresh()
		}
	case "agent_entry":
		// The watched child's new transcript entries, live. Only for the child
		// on screen, for the same reason as agent_history.
		if num(ev["agent"]) == m.scopeAgent && m.scopeAgent != 0 {
			items, _ := ev["items"].([]any)
			m.replayLive(items)
		}
	case "agent_told":
		// The ack says nothing the child's view does not, and it came first:
		// the told message reaches the transcript (agent_entry) only once the
		// child takes it, so "↳ Told agent" sat above what was told. Only a
		// refusal (the child is running, or stopped) is shown.
		if msg := str(ev["message"]); msg != "" && !strings.HasPrefix(msg, "Told agent") {
			m.append(stNote.Render("↳ " + textfmt.OneLine(msg)))
		}
	case "notification":
		switch str(ev["kind"]) {
		case "docs":
			m.convo = append(m.convo, convoEntry{docs: docsFromEvent(ev)})
			m.refresh()
		case "budget":
			// The turn budget ran out and the grace started: the machinery
			// speaking, so dim, not a bell.
			m.append(StDim.Render("⏱ " + textfmt.OneLine(str(ev["text"]))))
		case "schedule":
			// The scheduler speaking (a loop paced, or defaulted): dim, as
			// the machinery is.
			m.append(StDim.Render("⏲ " + textfmt.OneLine(str(ev["text"]))))
		case "peer":
			// Another dun session's dun_msg: a message box, the sender first
			// (the text is "<name> (<id>): <message>").
			if a := noticeMsg("peer", str(ev["text"])); a != nil {
				m.addMsg(a)
				m.refresh()
				break
			}
			m.append(StDim.Render("✉ dun msg from " + textfmt.OneLine(str(ev["text"]))))
		default:
			// A sub-agent's tell_parent message ("agent #2: …") is a box.
			if a := noticeMsg("", str(ev["text"])); a != nil {
				m.addMsg(a)
				m.refresh()
				break
			}
			m.convo = append(m.convo, noticeEntry(str(ev["text"]), m.noticeW()))
			m.refresh()
		}
	case "ask":
		m.endThinking()
		m.flushCur()
		m.paging = false // the question needs the screen
		m.jobViewing = false
		if !m.asking {
			m.keepDraft() // the composer is the user's draft; the question borrows it
		}
		m.asking, m.noting, m.customAnswer = true, false, false
		m.askSel, m.askNote = 0, ""
		m.input.Blur()
		m.settleAsk() // a new question replaces any parked one
		q := convoEntry{askText: str(ev["question"]), askOpen: true}
		q.renderAsk(false)
		m.convo = append(m.convo, q)
		m.refresh()
		m.askOptions = nil
		if opts, ok := ev["options"].([]any); ok {
			for _, o := range opts {
				m.askOptions = append(m.askOptions, fmt.Sprint(o))
			}
		}
		m.askMulti, _ = ev["multi"].(bool)
		m.askChecked = make([]bool, len(m.askOptions))
		// recommended_all lists them all (several only for multi); an older
		// engine sends just recommended.
		m.askRec = nil
		if all, ok := ev["recommended_all"].([]any); ok {
			for _, r := range all {
				m.askRec = append(m.askRec, fmt.Sprint(r))
			}
		} else if r := str(ev["recommended"]); r != "" {
			m.askRec = []string{r}
		}
		for i, o := range m.askOptions {
			switch {
			case !slices.Contains(m.askRec, o):
			case m.askMulti:
				m.askChecked[i] = true // checked to start with; done takes them
			default:
				m.askSel = i // enter takes the recommendation
			}
		}
		// No options → a pure free-text prompt: drop straight into text entry so
		// the user can just type. Otherwise typing is inert until enter opens the
		// "custom answer" row — surprising when there's nothing else to pick.
		if len(m.askOptions) == 0 {
			m.customAnswer = true
			m.input.Reset()
			m.input.placeholder = "type your answer…"
			m.input.Focus()
		}
	case "retry":
		return m.handleRetry(ev)
	case "upgrade":
		// dund is upgrading (to every connection, no session): a status row
		// of its own until the reconnect clears it.
		m.upgrade = parseUpgrade(ev)
		if m.upgrade.state == "failed" {
			// Nothing was replaced and dund runs on: said once, no row.
			m.append(stNote.Render(upgradeFailed(m.upgrade)))
			m.upgrade = nil
		}
		m.refresh()
	case "persist":
		// dund's answer to /persist.
		on, changed := ev["on"] == true, ev["changed"] == true
		switch {
		case on && changed:
			m.append(StDim.Render("persist: on — this session and its sub-agents keep running after /exit, and come back after a restart · /persist off to undo"))
		case on:
			m.append(StDim.Render("persist: on — /exit leaves this session running · /persist off to let /exit stop it"))
		case changed:
			m.append(StDim.Render("persist: off — /exit stops this session's engine; its sub-agents come back stopped (the conversation is kept)"))
		default:
			m.append(StDim.Render("persist: off — /exit stops this session's engine · /persist to keep it running"))
		}
	case "user":
		// The engine took a user message — from this client or another. It is
		// rendered here, not when sent, so every client shows the same
		// conversation. One buffered into the RUNNING turn is marked
		// provisional so it shows where it belongs — not stuck above the
		// divider; the marks clear when the turn ends.
		text := str(ev["content"])
		if ev["queued"] == true {
			m.convo = append(m.convo, convoEntry{
				collapsed:       StDim.Render("› " + text + " (queued)"),
				provisional:     true,
				provisionalText: text,
				userText:        text,
			})
		} else {
			m.task = text
			m.appendUser(text)
		}
		m.scrollPinned = true
		m.refresh()
	case "image_refused":
		// A message with images the engine did not send (images.go): why,
		// and the message back in the composer, ahead of anything typed since.
		m.append(StErr.Render("not sent: " + str(ev["message"])))
		var imgs []proto.Image
		if list, ok := ev["images"].([]any); ok {
			for _, it := range list {
				if o, ok := it.(map[string]any); ok {
					imgs = append(imgs, proto.Image{Path: str(o["path"]), Label: str(o["label"])})
				}
			}
		}
		m.input.Remember(imgs)
		draft := str(ev["content"])
		if cur := m.input.Value(); strings.TrimSpace(cur) != "" {
			draft += "\n" + cur
		}
		m.input.SetValue(draft)
		m.input.CursorEnd()
		m.refresh()
	case "turn":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
	case "answered":
		// The open question is settled, whoever answered it.
		// The client that answered has already closed it; on another, it is
		// still open and closes now.
		m.settleAsk()
		m.appendUser(str(ev["value"]))
		if m.asking {
			m = m.clearAsk()
		}
		m.refresh()
	case "commands":
		m.engineCmds = engineCmdsFromEvent(ev)
	case "settings":
		m.model, m.url = str(ev["model"]), str(ev["url"])
		if c, ok := ev["context"].(map[string]any); ok {
			m.ctxSettings = c
		}
		m.effort, m.effortWords = str(ev["effort"]), str(ev["effortWords"])
		if p := str(ev["provider"]); p != "" {
			m.provider = p
		}
		// The engine's word on whether it has anything to send to: the first
		// screen's one line, when it has not.
		m.noLogin = m.url == ""
		if m.splashWant {
			m.showSplash()
			m.splashWant, m.splashKeep, m.splashDone = false, false, true
		}
		if m.noLogin && !slices.ContainsFunc(m.convo, func(e convoEntry) bool { return e.needLogin }) {
			m.sayNeedLogin()
		}
		if !m.starting {
			m = m.sendKept()
		}
		if mode := str(ev["suggest"]); mode != "" {
			m.suggestMode = mode
		}
	case "queued":
		// The message itself arrived as a `user` event (queued); this is the
		// count of what the running turn has buffered.
		m.queuedMsgs = int(evNum(ev["count"]))
		// Track OOB messages for /context.
		m.ctxStats.oobMessages++
		m.syncStats()
		m.refresh()
	case "context_cost":
		// The pre-conversation cost, delivered when it is measured rather than
		// waiting for a turn to finish. Arrives twice: estimates, then exact.
		m.ctxStats.readSystemCost(ev)
		m.ctxStats.readWindow(ev)
		m.syncStats()
		m.refresh()
	case "spend":
		// A key-budget read that landed between turns (dun/spend.go).
		m.ctxStats.readSpend(ev)
		m.syncStats()
		m.refresh()
	case "usage":
		// Accumulate token usage stats for /context.
		m.ctxStats.totalTokens += int(evNum(ev["total"]))
		m.ctxStats.activeTokens = int(evNum(ev["active"]))
		m.ctxStats.cachedTokens += int(evNum(ev["cached"]))
		m.ctxStats.processedTokens += int(evNum(ev["processed"]))
		m.ctxStats.generatedTokens += int(evNum(ev["generated"]))
		m.ctxStats.turns = int(evNum(ev["turns"]))
		// Session-level stats (cumulative — use last value, not sum).
		m.ctxStats.readSystemCost(ev)
		m.ctxStats.readWindow(ev)
		m.ctxStats.readSideCalls(ev)
		m.ctxStats.readSurfacing(ev)
		m.ctxStats.readCache(ev)
		m.ctxStats.readSpend(ev)
		m.noteTurnStats(ev)
		if v := evNum(ev["forced_calls"]); v > 0 {
			m.ctxStats.forcedToolCalls = int(v)
		}
		if v := evNum(ev["notifications_lifted"]); v > 0 {
			m.ctxStats.notificationsSmuggled = int(v)
		}
		m.syncStats()
	case "done":
		cut := str(ev["reason"]) == "interrupted"
		if cut {
			m.markInterrupted()
		}
		m.endThinking()
		m.endRun()
		m.flushCur()
		// A question the turn asked ends with it, however the turn ended.
		if m.asking {
			m = m.clearAsk()
		}
		m.settleAsk()
		if cut {
			m.append(StDim.Render(interruptedRow))
			// The jobs the cut turn started keep running: say which, and how
			// to stop them.
			if d := strings.TrimSpace(str(ev["detail"])); d != "" {
				m.append(stNote.Render("  " + d))
			}
		}
		m.interrupting = false
		// Clear any remaining provisional markers — the turn is done, so
		// everything was delivered to the LLM. After an interrupt they were
		// not: they are the next turn's message, which the engine starts at
		// once, and its done settles them.
		for i := range m.convo {
			if !cut && m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		// Not re-pinned: a reader who scrolled up stays where they are (USER
		// 2026-10-28: zero bounce); a pinned one already follows.
		m.busy, m.queuedMsgs = false, 0
		m.busyStart = time.Time{}
		m.clearRetry()
		// The agent is idle: separate this turn's output from whatever the
		// human sends next with a blank row.
		m.turnGap()
		m.liveTurnDone = true
		// The turn is done, so the human's idle starts NOW. This is the only
		// place a suggestion request can begin — `done` is what "the LLM is
		// marked done" means on the wire, so nothing can land between a tool
		// call and its result.
		m.armIdleSuggest()
		m.refresh()
	case "error":
		m.endThinking()
		m.endRun()
		// A setup step not taken is the next step, not a failure: said as
		// the one action, with no retry advice — retrying cannot help.
		setup := false
		switch str(ev["error"]) {
		case dunconfig.ErrNoLogin.Error():
			m.sayNeedLogin()
			setup = true
		case dunconfig.ErrNoModel.Error():
			m.append(warnLine(dunconfig.ErrNoModel.Error()))
			setup = true
		default:
			m.append(StErr.Render("error: " + str(ev["error"])))
			m.attnErr = true // the window's "!" (tmux.go)
		}
		// Clear any remaining provisional markers — the engine flushes them on
		// error too, so they're part of the conversation history.
		for i := range m.convo {
			if m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		// Not re-pinned: a reader who scrolled up stays where they are (USER
		// 2026-10-28: zero bounce); a pinned one already follows.
		m.busy, m.queuedMsgs = false, 0
		m.busyStart = time.Time{}
		m.clearRetry()
		// Whether the SESSION survived is the engine's call, not something to
		// infer from the error text. It says so, because promising "send a
		// message to retry" to a session that cannot run another turn is worse
		// than saying nothing.
		if b, _ := ev["fatal"].(bool); b {
			m.append(StDim.Render("the session ended — resume it with: dun --continue"))
		} else if !setup {
			// The conversation is on disk, so the next message pairs off whatever
			// was interrupted and picks up where this stopped.
			m.append(StDim.Render("the session is intact — send a message to retry from here"))
		}
		m.turnGap()
		m.refresh()
	case "reset":
		// The conversation was cleared — by this client's /clear or another's.
		m.convo = nil
		m.cur = ""
		m.pendingTool = -1
		m.activeTool = ""
		m.sel = -1
		m.busy = false
		m.dropAsk()
		m.dropHold()                         // nothing left on screen to hold in place
		m.suggestions, m.suggestSel = nil, 0 // they were about the conversation just cleared
		m.settleAsk()
		m.append(StDim.Render("session cleared — fresh start"))
		m.refresh()
	case "compaction":
		// On screen, not just in a log a TUI never shows: this is the one
		// operation that DESTROYS conversation, and it used to happen silently.
		m.append(stNote.Render("🗜 " + str(ev["text"])))
		// Track for /context: compute saved from before/after
		m.ctxStats.compactions++
		before := evNum(ev["tokens_before"])
		after := evNum(ev["tokens_after"])
		if before > after {
			m.ctxStats.tokensCompacted += int(before - after)
		}
		m.refresh()
	case "engine":
		// dund supervises the engine now; this is what it reports.
		switch str(ev["state"]) {
		case "started":
			m.fatalErr, m.exitAnnounced = "", false
			// A new engine: input waits for its ready.
			if !m.starting {
				m.starting, m.startingStart = true, time.Now()
			}
		case "restarting":
			m.busy = false
			m.dropAsk()
			m.settleAsk()
			m.busyStart = time.Time{}
			m.restarts = int(evNum(ev["restarts"]))
			m.skipHistory = true // it will replay what is already on screen
			m.append(StErr.Render(str(ev["reason"]) + " — restarting it; the session is kept"))
		case "gave_up":
			m.busy, m.starting = false, false
			m.dropAsk()
			m.settleAsk()
			m.fatalErr = str(ev["reason"]) + " — gave up restarting it"
			m.append(StErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved"))
		case "stopped":
			m.busy = false
			m.dropAsk()
			m.settleAsk()
		}
		m.refresh()
	case "ui_state":
		m = m.applyUI(ev["state"])
	case "snapshot":
		// Too far behind to replay: the conversation, then the latest state.
		// On a reconnect the conversation is already on screen; the history a
		// respawned engine sends next is skipped by the same flag.
		if !m.skipHistory {
			items, _ := ev["items"].([]any)
			m.replay(items)
		}
		state, _ := ev["state"].([]any)
		for _, st := range state {
			if sev, ok := st.(map[string]any); ok && sev["type"] != "history" {
				m = m.handleEvent(evMsg(sev))
			}
		}
		m.refresh()
	case "exit":
		// The engine only announces an exit it CHOSE — ctrl-C, an explicit stop,
		// stdin closing. That is the difference between "it left" and "it died",
		// and it decides whether the TUI puts a new one in its place.
		m.exitAnnounced = true
		if r := str(ev["reason"]); r != "" {
			m.fatalErr = "dun engine exited: " + r
		}
	}
	return m
}

// handleRetry renders the provider-wait state: a live banner while dun is backing
// off, a scrollback line for the start and the outcome.
//
// Only the FIRST wait of an outage goes to scrollback. The banner carries the
// attempt number and counts down, so awareness lives there; logging every attempt
// would bury the conversation under a hundred identical lines during a long
// outage.
func (m tuiModel) handleRetry(ev evMsg) tuiModel {
	switch str(ev["kind"]) {
	case "recovered":
		m.clearRetry()
		// Update the retry block in-place if it exists.
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].run == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
			e := &m.convo[len(m.convo)-1]
			e.collapsed = StTool.Render("✓ " + str(ev["text"]))
			e.full = ""
			e.state = viewMinimized
		} else {
			m.append(StTool.Render("✓ " + str(ev["text"])))
		}
	case "giveup":
		m.clearRetry()
		m.busy = false
		m.busyStart = time.Time{}
		// Update the retry block in-place if it exists.
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].run == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
			e := &m.convo[len(m.convo)-1]
			e.collapsed = StErr.Render("✗ " + str(ev["text"]))
			e.full = ""
			e.state = viewMinimized
		} else {
			m.append(StErr.Render("✗ " + str(ev["text"])))
		}
	default:
		// A turn-scope retry means the generation died mid-stream and will be
		// redone, so drop the half-streamed text rather than letting the regenerated
		// reply append to a broken sentence. Tool results already recorded are kept
		// (the engine resumes from them), only the interrupted reply is discarded.
		if str(ev["scope"]) == "turn" {
			if replyShown(m.cur) {
				// Its text goes (it is being regenerated), its rows do not: a
				// marker holds the height it had.
				m.convo = append(m.convo, convoEntry{collapsed: StDim.Render("↺ the reply was cut off — regenerating"),
					maxH: m.curMaxH, maxKey: m.curMaxKey})
			}
			m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
		}
		// Whatever was thought before the failure stays, folded; the retried
		// round thinks in a block of its own.
		m.endThinking()
		m.busy = true
		m.retry = m.retryBanner(ev)
		if ms := evNum(ev["delay_ms"]); ms > 0 {
			m.retryDue = time.Now().Add(time.Duration(ms) * time.Millisecond)
		} else {
			m.retryDue = time.Time{}
		}
		if m.retrySeen == 0 {
			// First retry: create a collapsible block with a brief collapsed line
			// and full formatted details.
			collapsed := stNote.Render("⏳ " + str(ev["reason"]))
			full := m.retryDetails(ev)
			m.convo = append(m.convo, convoEntry{
				collapsed: StDim.Render("▸ ") + collapsed,
				full:      full,
			})
		} else {
			// Subsequent retry: update the existing block's full details.
			if len(m.convo) > 0 {
				e := &m.convo[len(m.convo)-1]
				e.full = m.retryDetails(ev)
				e.collapsed = StDim.Render("▸ ") + stNote.Render("⏳ "+str(ev["reason"]))
				e.invalidateWrap()
			}
		}
		m.retrySeen++
	}
	m.refresh()
	return m
}

// retryDetails builds the expanded view for a retry block: formatted fields
// instead of a raw JSON payload.
func (m tuiModel) retryDetails(ev evMsg) string {
	var lines []string
	reason := str(ev["reason"])
	if reason != "" {
		lines = append(lines, stNote.Render("reason:  ")+reason)
	}
	if detail := str(ev["detail"]); detail != "" {
		lines = append(lines, StDim.Render("detail:  ")+detail)
	}
	if cap := evNum(ev["capacity"]); cap > 0 {
		lines = append(lines, StDim.Render(fmt.Sprintf("capacity:  %d/%d busy  (%d ahead)",
			int(evNum(ev["in_flight"])), int(cap), int(evNum(ev["waiting"])))))
	}
	if queue := str(ev["queue"]); queue != "" {
		lines = append(lines, StDim.Render("queue:   ")+queue)
	}
	if attempt := evNum(ev["attempt"]); attempt > 0 {
		lines = append(lines, StDim.Render(fmt.Sprintf("attempt: %d", int(attempt))))
	}
	if delay := evNum(ev["delay_ms"]); delay > 0 {
		lines = append(lines, StDim.Render(fmt.Sprintf("retry in:  %s", time.Duration(delay)*time.Millisecond)))
	}
	if elapsed := evNum(ev["elapsed_ms"]); elapsed > 0 {
		lines = append(lines, StDim.Render(fmt.Sprintf("elapsed:   %s", time.Duration(elapsed)*time.Millisecond)))
	}
	if budget := evNum(ev["budget_ms"]); budget > 0 {
		lines = append(lines, StDim.Render(fmt.Sprintf("budget:    %s", time.Duration(budget)*time.Millisecond)))
	}
	if str(ev["server_asked"]) == "true" {
		lines = append(lines, StDim.Render("server asked for this delay"))
	}
	return strings.Join(lines, "\n")
}

// retryBanner is the status-line text for a wait in progress: what the provider
// said, including the queue numbers when it sent them (corrallm does).
func (m tuiModel) retryBanner(ev evMsg) string {
	b := str(ev["reason"])
	if cap := evNum(ev["capacity"]); cap > 0 {
		b += fmt.Sprintf(" · %d/%d busy", int(evNum(ev["in_flight"])), int(cap))
	}
	if w := evNum(ev["waiting"]); w > 0 {
		b += fmt.Sprintf(" · %d ahead", int(w))
	}
	return b
}

// clearRetry takes the banner down.
func (m *tuiModel) clearRetry() {
	m.retry, m.retryDue, m.retrySeen = "", time.Time{}, 0
}

// evNum reads a JSON number out of an event (they decode as float64).
func evNum(v any) float64 {
	f, _ := v.(float64)
	return f
}

// retryCountdown is the " · next try in 7s" tail of the retry banner. Recomputed
// on every spinner tick, which is what makes the wait visibly a wait rather than
// a hang.
func (m tuiModel) retryCountdown() string {
	if m.retryDue.IsZero() {
		return ""
	}
	left := time.Until(m.retryDue)
	if left <= 0 {
		return " · retrying now"
	}
	return fmt.Sprintf(" · next try in %s", left.Round(time.Second))
}

// startingElapsed returns the " · 3s" elapsed tail for the spawning banner.
func (m tuiModel) startingElapsed() string {
	if m.startingStart.IsZero() {
		return ""
	}
	return fmt.Sprintf(" · %s", time.Since(m.startingStart).Round(time.Second))
}

// busyElapsed returns the " · 3s" elapsed tail for the generic busy banner.
func (m tuiModel) busyElapsed() string {
	if m.busyStart.IsZero() {
		return ""
	}
	return fmt.Sprintf(" · %s", time.Since(m.busyStart).Round(time.Second))
}

// queuedHint reports messages typed mid-turn that are waiting to be lifted into
// the next tool result, so the user knows they landed.
func (m tuiModel) queuedHint() string {
	switch m.queuedMsgs {
	case 0:
		return ""
	case 1:
		return " (1 message queued for this turn)"
	}
	return fmt.Sprintf(" (%d messages queued for this turn)", m.queuedMsgs)
}

// exitHint is the status-bar exit prompt — "/exit to exit" when ctrl+c is
// disabled (--disable-exit), else the usual "ctrl+c quit".
func (m tuiModel) exitHint() string {
	if m.disableExit {
		return "/exit to exit"
	}
	return "ctrl+c quit"
}
func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}
func floatOf(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// docsFromEvent builds a docsBlock from a `notification` event of kind "docs".
func docsFromEvent(ev evMsg) *docsBlock {
	d := &docsBlock{found: intOf(ev["found"]), surfaced: intOf(ev["surfaced"]), tokens: intOf(ev["tokens"])}
	if arr, ok := ev["docs"].([]any); ok {
		for _, it := range arr {
			dm, _ := it.(map[string]any)
			d.docs = append(d.docs, docNode{title: str(dm["title"]), line: str(dm["line"]), score: floatOf(dm["score"]),
				source: str(dm["source"]), why: str(dm["why"]), kind: str(dm["kind"])})
		}
	}
	return d
}
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// evParts decodes the context breakdown rows off a usage event. A row that is
// missing or malformed is dropped rather than rendered as zero, because a
// zero-token MCP server on the /context table reads as a real measurement.
func evParts(v any) []dun.ContextPart {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]dun.ContextPart, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out = append(out, dun.ContextPart{Name: name, Tokens: int(evNum(m["tokens"]))})
	}
	return out
}
