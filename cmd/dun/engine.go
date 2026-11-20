package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// skillPoll is how often the engine looks for changed skill files.
var skillPoll = 3 * time.Second

func runProgrammatic(ctx context.Context, h *harness.Harness, em *emitter, in *inputStream, peers *peerLink, firstTask string, cc closeCtx) {
	// Stop the turn in flight (proto.Interrupt, /interrupt). On the reader
	// goroutine: it only cancels a context, and the turn loop reports the end
	// with done{reason: interrupted}. Installed before ready, so a client that
	// has seen ready can count on it.
	interruptCmd := func(reason string) {
		if !h.Interrupt(reason) {
			em.emit(&proto.ControlEvent{ID: "interrupt", Message: "nothing to interrupt: no turn is running"})
		} else if em.turn != nil {
			em.turn.interrupting()
		}
	}
	in.setInterrupt(interruptCmd)
	em.emit(&proto.Ready{Version: buildinfo.Version, Tools: h.ToolNames(), Servers: enginecmd.ServersProto(h.Servers()),
		Hint: withToolWarnings(enginecmd.ServerHint(h.Servers()))})
	em.emit(catalogEvent(h))
	em.emit(settingsEvent())
	em.emit(commandsEvent(h))
	// Server commands (/rag, /lsp) are handled on the READER's goroutine, not
	// here: this loop is blocked inside a turn most of the time, and "turn the
	// docs server on" should not queue behind a five-minute agent run. The
	// harness defers the actual tool-set swap to a turn boundary.
	srvCmd := func(alias, action string) {
		run := func() {
			msg := runServerCmd(ctx, h, alias, action)
			em.emit(&proto.ServerEvent{ID: alias, Action: action, Message: msg,
				Servers: enginecmd.ServersProto(h.Servers()), Tools: h.ToolNames(), Builtins: enginecmd.ServersProto(h.BuiltinGroups())})
			em.emit(catalogEvent(h))
			em.emit(commandsEvent(h))
		}
		// Never on the reader goroutine, which answers asks and takes the
		// next message: a start takes up to its timeout (an OAuth one, a
		// person in a browser), and a switch changed while it starts (off)
		// must not wait behind it (USER 2026-10-25).
		go run()
	}
	in.setServerCmd(srvCmd)
	// A skill added, removed or edited on disk reaches the model's skill
	// list and every client's palette within a few seconds, not at the next
	// restart.
	h.CheckSkills() // records what is there now
	go func() {
		t := time.NewTicker(skillPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if h.CheckSkills() {
				em.emit(catalogEvent(h))
				em.emit(commandsEvent(h))
			}
		}
	}()
	resetCmd := func() {
		h.Reset()
		em.emit(&proto.ResetEvent{})
	}
	var ctrlCmd func(id, action string)
	ctrlCmd = func(id, action string) {
		// A slash line from a client that is not the TUI (web, phone): routed
		// here, by the same handlers the TUI's commands reach. See commands.go.
		if id == "slash" {
			exts, _ := engineExtensions(h.Extensions())
			routeSlash(action, slashRoutes{server: srvCmd, control: ctrlCmd, reset: resetCmd, exts: exts,
				reply: func(msg string) { em.emit(&proto.ControlEvent{ID: "slash", Action: action, Message: msg}) }})
			return
		}
		// /ext lists the extensions; "call <id> <command> <text>" is an
		// extension's slash command, a tool call — detached, as any server
		// round trip is. "watch|attach|detach <id> <args>" are the index
		// commands (extIndexCmd).
		if id == "ext" && action == "view" { // the /ext list (extview.go)
			go func() { em.emit(extensionsEvent(h)) }()
			return
		}
		if id == "ext" {
			go func() {
				msg := extListing(h)
				if f := strings.SplitN(action, " ", 3); len(f) >= 2 && (f[0] == "watch" || f[0] == "attach" || f[0] == "detach" || f[0] == "workspace") {
					rest := ""
					if len(f) == 3 {
						rest = f[2]
					}
					msg = extIndexCmd(ctx, h, f[0], f[1], rest)
				} else if f := strings.SplitN(action, " ", 4); len(f) >= 3 && f[0] == "call" {
					text := ""
					if len(f) == 4 {
						text = f[3]
					}
					out, err := h.RunExtCommand(ctx, f[1], f[2], text)
					if err != nil {
						out = "error: " + err.Error()
					}
					msg = "/" + f[1] + " " + f[2] + ": " + out
				}
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			}()
			return
		}
		if id == "interrupt" {
			interruptCmd("")
			return
		}
		// /schedule and /loop (schedule.go): dund keeps them, so the answer
		// is a round-trip over the peer link — detached, because dund's reply
		// comes in on this reader goroutine.
		if id == "schedule" || id == "loop" {
			go func() {
				if peers == nil {
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: "/" + id + " " + noDund})
					return
				}
				if id == "loop" {
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: loopCmd(ctx, peers.sched, action, time.Now())})
					return
				}
				msg, edit := schedCmd(ctx, peers.sched, action, time.Now())
				if edit != "" {
					// The TUI that asked puts it in its composer (tui.go).
					em.emit(&proto.ControlEvent{ID: "schedule-edit", Action: edit, Message: msg})
					return
				}
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			}()
			return
		}
		// /jobs: list or kill background jobs. Detached: a kill waits up to a
		// few seconds for the job's process group to be gone.
		if id == "jobs" {
			go func() { em.emit(&proto.ControlEvent{ID: id, Action: action, Message: jobsCmd(h, action)}) }()
			return
		}
		// /rephrase flips a pure engine-side toggle: no harness work, no asking,
		// so it does not go through runControlCmd. The TUI already echoed the
		// command to the user; this event keeps the engine's and the TUI's
		// knowledge in sync (a bare /rephrase from the TUI still round-trips).
		if id == "rephrase" && strings.HasPrefix(action, "prompt") {
			text := strings.TrimSpace(strings.TrimPrefix(action, "prompt"))
			var msg string
			switch text {
			case "":
				p, own := harness.RephrasePrompt()
				who := "dun's"
				if own {
					who = "yours"
				}
				msg = "rephrase prompt (" + who + "; /rephrase prompt <text> sets it, /rephrase prompt default goes back):\n" + strings.TrimSuffix(p, "\n\nUser's message:\n")
			case "default":
				if err := harness.SetRephrasePrompt(""); err != nil {
					msg = "rephrase prompt: " + err.Error()
				} else {
					msg = "rephrase prompt: dun's again"
				}
			default:
				if err := harness.SetRephrasePrompt(text); err != nil {
					msg = "rephrase prompt: " + err.Error()
				} else {
					msg = "rephrase prompt: saved (" + numfmt.Grouped(len(text)) + " chars); the user's message follows it"
				}
			}
			em.emit(&proto.ControlEvent{ID: id, Action: "prompt", Message: msg})
			return
		}
		if id == "rephrase" {
			enginecmd.RephraseOn = action == "on"
			em.emit(settingsEvent())
			em.emit(commandsEvent(h))
			return
		}
		// /close is handled here rather than in runControlCmd because it is
		// about the SESSION, not the harness: it needs the workspace root and
		// the session id, which only this scope has.
		if id == "close" {
			em.emit(&proto.ControlEvent{ID: id, Action: action,
				Message: closeSession(cc.workspace, cc.sessionID, cc.wt, cc.rag)})
			return
		}
		// /suggest: the ONLY thing that asks for suggestions now. The engine no
		// longer volunteers them at the end of a turn — the UI knows what the
		// engine cannot (whether anyone is typing), so it decides when to ask.
		//
		// Detached, for the same reason an asking command is: this is the reader
		// goroutine, and h.Suggestions is a full LLM round-trip. Running it here
		// blocked every `answer` and `user` event for the length of a model call.
		if id == "suggest" {
			switch action {
			case "on", "off", "auto":
				enginecmd.SuggestMode = action
				em.emit(settingsEvent())
				em.emit(commandsEvent(h))
			default:
				go emitSuggestions(ctx, h, em)
			}
			return
		}
		// /remember: the memory fork, a full LLM round-trip — detached for the
		// same reason as suggest. The action is the user's text, not a keyword.
		if id == "remember" {
			go func() {
				msg, err := h.Remember(ctx, action)
				if err != nil {
					msg = "nothing saved: " + err.Error()
				}
				msg = "memory: " + msg
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			}()
			return
		}
		// /mcp add and /mcp remove: detached, because starting a server is a
		// handshake and a tool listing over the network. The action carries
		// the spec — bearer token included — so the event echoes only the
		// verb, never the action itself.
		if id == "mcp" {
			go func() {
				verb, _, _ := strings.Cut(action, " ")
				msg := runMCPCmd(ctx, h, action)
				em.emit(&proto.ServerEvent{ID: "mcp", Action: verb, Message: msg,
					Servers: enginecmd.ServersProto(h.Servers()), Tools: h.ToolNames(), Builtins: enginecmd.ServersProto(h.BuiltinGroups())})
				em.emit(catalogEvent(h))
				em.emit(commandsEvent(h))
			}()
			return
		}
		// The model list, for a client's model picker: a network round trip
		// per provider, so off the reader goroutine.
		if id == "models" {
			go func() {
				providersMu.Lock()
				defer providersMu.Unlock()
				em.emit(modelsEvent())
			}()
			return
		}
		// /model add, edit, remove, use from the web or the phone: the action
		// is a providerOp, key included, so the event echoes only the verb.
		if id == "provider" {
			go func() {
				providersMu.Lock()
				defer providersMu.Unlock()
				var op dunconfig.ProviderOp
				msg := ""
				if err := json.Unmarshal([]byte(action), &op); err != nil {
					msg = "a provider operation is JSON: " + err.Error()
				} else if m, err := dunconfig.ApplyProviderOp(op); err != nil {
					msg = err.Error()
				} else {
					msg = m
				}
				em.emit(&proto.ControlEvent{ID: id, Action: op.Op, Message: msg})
				em.emit(modelsEvent())
			}()
			return
		}
		// /mcp info: the servers, their cost, the shell and what scripting
		// saves (mcpinfo.go). Detached, as /mcp tools is: the server list
		// waits on the server lock a start holds.
		if id == "mcpinfo" {
			go func() {
				bd := h.SystemParts()
				// The session's own speed, else the ledger's recent rounds'.
				usage := h.StatsSummary(time.Now().AddDate(0, 0, -mcpInfoDays))
				speed, n := h.PrefillSpeed()
				if n == 0 {
					speed, n = usage.Speed, usage.SpeedRounds
				}
				em.emit(&proto.ControlEvent{ID: id, Message: enginecmd.McpInfo(enginecmd.McpInfoIn{Servers: h.Servers(), Parts: bd.Parts,
					Counted: bd.Total > 0, Exact: bd.Exact, Window: h.Window().Window,
					Usage: usage, UsageDays: mcpInfoDays, Speed: speed, SpeedRounds: n})})
			}()
			return
		}
		// /mcp tools: one server's tools, hidden ones included, or a toggle.
		// Detached: a toggle writes a file and may rebuild the tool set.
		if id == "mcptools" {
			go func() { em.emit(mcpToolsEvent(h, action)) }()
			return
		}
		// A kind of scoped switches (skills): its view, after an operation
		// on it when the action has one (switches.go). Detached: a change
		// rebuilds the tool set, which waits for a running turn.
		if id == "switches" {
			go func() { em.emit(switchesEvent(h, action)) }()
			return
		}
		// One file of a skill, for the /skill browser (skillpane.go).
		// Detached: the skill list waits on the server lock.
		if id == "skillfile" {
			go func() { em.emit(skillFileEvent(ctx, h, action)) }()
			return
		}
		// /context config [--max N] [--compact-buffer N] [--tool-lod N]
		// [--recap-nudge N] [--ceiling N]: saved for this model (dun/window.go);
		// bare, the settings in force.
		if id == "ctxconfig" {
			go func() {
				msg, _, err := contextConfigCmd(ctx, h, action)
				if err != nil {
					msg = "context config: " + err.Error()
				}
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
				em.emit(settingsEvent())
			}()
			return
		}
		// /worklog: the session's tree of tasks (dun/worklog.go). The engine
		// owns it; write is detached, since it waits on one model call.
		if id == "worklog" {
			verb, rest, _ := strings.Cut(strings.TrimSpace(action), " ")
			if strings.EqualFold(verb, "write") {
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: "worklog: writing the account…"})
				go func() {
					msg := ""
					path, err := h.WriteWorklog(ctx, rest)
					if err != nil {
						msg = "worklog: " + err.Error()
					} else {
						msg = "worklog: account written to " + path
					}
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
				}()
				return
			}
			msg, err := h.WorklogCmd(action)
			if err != nil {
				msg = "worklog: " + err.Error()
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			return
		}
		// /journal: what the agent did, in time order (dun/journal.go).
		// "screen" is the TUI's data; now is detached, since it waits on
		// one model call.
		if id == "journal" {
			verb := strings.ToLower(strings.TrimSpace(action))
			switch verb {
			case "screen":
				em.emit(journalEvent(h))
				return
			case "now":
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: "journal: writing an entry…"})
				go func() {
					e, n, err := h.JournalNow(ctx)
					msg := ""
					if err != nil {
						msg = "journal: " + err.Error()
					} else {
						msg = "journal: " + e.Line(n) + " · /journal view " + strconv.Itoa(n)
					}
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
					em.emit(journalEvent(h)) // an open screen refreshes
				}()
				return
			}
			msg, err := h.JournalCmd(action)
			if err != nil {
				msg = "journal: " + err.Error()
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			return
		}
		// /plan: the task plan (dun/plan.go). "view" is the screen's
		// data; the rest are the person's changes, answered with the plan
		// again so an open screen refreshes.
		if id == "plan" {
			if action == "view" {
				em.emit(planEvent(h, ""))
				return
			}
			// summary: one side call, detached, beside any turn
			// (dun/plansummary.go). The plan events it sends carry
			// "summarizing…" and then the summary; the reply is the text,
			// for a client with no screen.
			if strings.EqualFold(strings.TrimSpace(action), "summary") {
				go func() {
					s, err := h.SummarizePlan(ctx)
					msg := ""
					if err != nil {
						msg = "plan summary: " + err.Error()
					} else {
						msg = "plan summary · " + s.Made() + "\n\n" + s.Text
					}
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
				}()
				return
			}
			msg, err := h.PlanCmd(ctx, action)
			if err != nil {
				msg = "plan: " + err.Error()
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			em.emit(planEvent(h, ""))
			return
		}
		// /file: paths outside the workspace the file tools may use, at a
		// scope, each read or write and indexed or not (dun/fileadd.go).
		if id == "files" {
			msg, err := fileCmd(h, action)
			if err != nil {
				msg = "file: " + err.Error()
			} else if strings.TrimSpace(action) != "" {
				h.Aside(h.FilesAside()) // the model must know read from write
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			return
		}
		// The TUI's throttled sign that the person is here: a pending settle
		// starts its delay again (dun/settle.go). No reply: it is not news.
		if id == "presence" {
			h.SettlePresence()
			return
		}
		// Up with messages queued (the TUI): take back the ones the running
		// turn has not taken, for the composer.
		if id == "unqueue" {
			ids := []string{}
			for _, q := range h.Unqueue() {
				ids = append(ids, q.ID)
			}
			em.emit(&proto.Withdrawn{IDs: ids})
			em.emit(&proto.Queued{Count: h.Queued()})
			return
		}
		// /retry: bare says the settings; a preset or a knob is set on the
		// client at once and saved (retrycmd.go).
		if id == "retry" {
			c := h.LLMClient()
			if c == nil {
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: "retry: this session's model client has no retry settings"})
				return
			}
			msg, save, err := retryCmd(c, action)
			if err != nil {
				msg = "retry: " + err.Error()
			} else if save != nil {
				if err := dunconfig.SaveConfig(*save); err != nil {
					msg += " — but not saved: " + err.Error()
				}
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			return
		}
		// /loops: bare says the settings and what was found; a mode or a knob
		// applies from the next reply and is saved (loopscmd.go).
		if id == "loops" {
			msg, save, err := loopWatch.command(action)
			if err != nil {
				msg = "loops: " + err.Error()
			} else if save != nil {
				if err := dunconfig.SaveConfig(*save); err != nil {
					msg += " — but not saved: " + err.Error()
				}
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			return
		}
		// /spend: real spend and the budget; the login needs a terminal, so it
		// says the command (spendcmd.go).
		if id == "spend" {
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: spendControl(ctx, action)})
			return
		}
		// /model caps | probe: what the session's model is known to do, and
		// the explicit check (capscmd.go). Detached: a probe is requests.
		if id == "model" {
			go func() { em.emit(&proto.ControlEvent{ID: id, Action: action, Message: capsControl(ctx, action)}) }()
			return
		}
		// /effort: bare says the level; a level is set, saved, and applies
		// from the next request (dun/effort.go).
		if id == "effort" {
			msg := "effort: " + h.Effort() + " — " + h.EffortWords(h.Effort())
			if action != "" {
				if err := h.SetEffort(action); err != nil {
					msg = "effort: " + err.Error()
				} else {
					enginecmd.EffortLevel = h.Effort()
					msg = "effort: " + enginecmd.EffortLevel + " — " + h.EffortWords(enginecmd.EffortLevel) + " (saved; from the next request)"
				}
			}
			// The saved level is kept; what this model is sent is said beside it.
			if eff, why := h.EffortSent(); eff != h.Effort() {
				msg += "; sent as " + eff
				if why != "" {
					msg += ": " + why
				}
			}
			em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			em.emit(commandsEvent(h))
			em.emit(settingsEvent())
			return
		}
		// /rules: bare reports; "full|summary|index" saves the project's
		// mode (with a scope, that scope's); on|off|unset <file> and mode
		// <mode> [--scope] are the rules switches' (switchesengine.go).
		// Detached: summary is a model round trip, and every change rebuilds
		// the tool set, which waits for a running turn.
		if id == "rules" {
			go func() {
				msg, err := rulesCmd(ctx, h, action)
				if err != nil {
					msg = "rules: " + err.Error()
				}
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			}()
			return
		}
		// /repaint report: the action is a capture directory; the model call
		// reads its prompt.txt and writes model-output.txt (repaint.go).
		// Detached: a model round trip. The event carries the directory or
		// the error, never the capture.
		if id == "repaint" {
			go func() {
				if err := h.RepaintReport(ctx, action); err != nil {
					em.emit(&proto.ControlEvent{ID: id, Action: "error", Message: "repaint report: " + err.Error()})
					return
				}
				em.emit(&proto.ControlEvent{ID: id, Action: "done", Message: "repaint report: model output in " + action})
			}()
			return
		}
		// /memories: the project's notes, strategies and constraints and
		// what has happened to them; add, rm, important, unimportant.
		if id == "memories" {
			if strings.TrimSpace(action) == "" {
				em.emit(&proto.ControlEvent{ID: id, Message: memoriesListing(h.Memories(), h.Remembering())})
				return
			}
			msg, err := h.MemoriesCommand(action)
			if err != nil {
				msg = "memories: " + err.Error()
			}
			em.emit(&proto.ControlEvent{ID: id, Message: msg})
			return
		}
		// An MCP prompt, run as a slash command: rendered here, sent back for
		// the UI to submit as the user's message — so it is echoed, queued and
		// recorded exactly like one typed.
		if id == "prompt" {
			go func() {
				var p struct {
					Server string            `json:"server"`
					Name   string            `json:"name"`
					Args   map[string]string `json:"args"`
				}
				text, err := "", json.Unmarshal([]byte(action), &p)
				if err == nil {
					text, err = h.RenderPrompt(ctx, p.Server, p.Name, p.Args)
				}
				if err != nil {
					em.emit(&proto.ControlEvent{ID: id, Message: "prompt " + p.Server + ":" + p.Name + ": " + err.Error()})
					return
				}
				em.emit(&proto.PromptText{Text: text})
			}()
			return
		}
		// A control command that ASKS cannot run here. This callback is on the
		// reader goroutine, which is the only reader of `answer` events — block
		// it on a question and the answer can never arrive, so the session hangs
		// with its own prompt on screen. Detach it: the command runs to
		// completion on its own goroutine and emits when it is done, which is
		// also what lets the human keep typing while deciding.
		//
		// The trade is that a `quit` arriving while the question is open can
		// outrun the reply. That is the right way round — the alternative is a
		// session that cannot be quit until someone answers — and the ask itself
		// unblocks on ctx, so nothing is left hanging.
		if ctrlCmdAsks(id, action) {
			go func() {
				if isolateBuilds(id, action) {
					em.emit(&proto.ControlEvent{ID: id, Action: action, Message: "isolate: setting the docker image (a Dockerfile is built; this can take a while) ..."})
				}
				msg := runControlCmd(ctx, h, id, action)
				em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
			}()
			return
		}
		wsBefore, isoBefore := h.Workspace(), isolationMode(h)
		msg := runControlCmd(ctx, h, id, action)
		em.emit(&proto.ControlEvent{ID: id, Action: action, Message: msg})
		for _, ev := range isolateEvents(h, wsBefore, isoBefore) {
			em.emit(ev)
		}
	}
	in.setCtrlCmd(ctrlCmd)
	in.setResetCb(resetCmd)
	// Agent scope: the human reading a child's conversation, or steering it.
	// `view` sends the scrollback (a stopped child's too) and starts the
	// watch that forwards its new entries; `leave` ends it.
	if peers != nil {
		// A global server change made here reaches the owner's other
		// sessions (proto.ServersChanged); not waited for.
		mcpserver.OnUserServersWritten = func() {
			go func() { _, _ = peers.call(context.Background(), proto.PeerRequest{Op: "servers"}) }()
		}
	}
	in.setPeerFn(peerIn(peers, h, em))
	em.mu.Lock()
	em.onReply = recordReplies(h) // the person's commands, kept in the session
	em.mu.Unlock()
	in.setAgentFn(func(a proto.AgentCmd) {
		n, err := strconv.Atoi(a.ID)
		if err != nil {
			return
		}
		content := a.Content
		switch a.Action {
		case "view":
			// The history, then each new entry as an agent_entry until
			// "leave": what makes the view live while the child works.
			h.AgentWatch(n, func(items []harness.HistoryItem) {
				em.emit(&proto.AgentHistory{Agent: n, Prompt: h.AgentPrompt(n), Items: historyProto(items)})
			})
		case "leave":
			h.AgentUnwatch(n)
		case "tell":
			// Images as the session takes them (turn): refused up front when
			// the child's model cannot take them, the message back in the
			// composer. No done: the client opened no turn for it.
			if len(a.Images) > 0 {
				if why := h.AgentImagesRefused(n); why != "" {
					em.emit(&proto.ImageRefused{Message: why, Content: content, Images: a.Images})
					return
				}
			}
			em.emit(&proto.AgentTold{Agent: n, Message: h.AgentTellImages(n, content, dunImages(a.Images))})
		case "continue": // dund, after a restart that cut the child's run
			em.emit(&proto.AgentTold{Agent: n, Message: h.AgentContinue(n, content)})
		}
	})
	// A message that arrives while a turn is running does NOT wait for it. It is
	// buffered and lifted into the next tool result, so the model reads it inside
	// the turn it is already running — and several of them batch. Only when no turn
	// is in flight does a message start one (the channel path below). The gate
	// (harness/turngate.go) is the one a sub-agent's tell goes through too.
	in.setMidTurn(func(msg userMsg) bool {
		mid := harness.NewMessageID()
		um := harness.UserInput{ID: mid, Text: msg.content, Images: dunImages(msg.images)}
		// Echo BEFORE Say: Say can release an exec wait, and the delivered
		// event that follows its result names this id.
		switch h.Gate().Offer(h, um, func() { em.emit(&proto.UserEcho{ID: mid, Content: msg.content, Queued: true}) }) {
		case harness.OfferHeld:
			em.emit(&proto.Notification{Text: "a message with images waits for this turn to end: an image cannot ride a tool result"})
			return true
		case harness.OfferQueued:
			em.emit(&proto.Queued{Text: msg.content, Count: h.Queued()})
			return true
		}
		return false
	})
	if firstTask != "" {
		// A task given on the command line is the whole run: it waits for
		// the servers still starting, so it has every tool it was started
		// with. A person typing waits for nothing (harness.Start).
		h.WaitServers(ctx)
		turn(ctx, h, em, firstTask, nil)
	}
	users := in.users
	// A turn that fails must not end the session — the user is still sitting
	// there and the conversation is still on disk. But the pending work that
	// turn was going to claim is still pending, so retrying it immediately would
	// spin: provider down, turn fails, still pending, turn fails… autoContinue
	// stops that. It is re-armed by anything NEW arriving (a message, a
	// background completion), which is exactly when a retry is worth making.
	autoContinue := true
	for {
		// A message with images held during the last turn starts the next.
		if msg, ok := h.Gate().TakeHeld(); ok {
			turn(ctx, h, em, msg.Text, protoImages(msg.Images))
			autoContinue = true
			continue
		}
		// Anything buffered while the last turn ran (or after it failed on the
		// provider) is delivered now, in ONE turn — including a message the user
		// sent to nudge a dead session back to life.
		if autoContinue && h.Pending() > 0 {
			if !mayWake() { // dund is upgrading: kept for after the restart
				holdWakes(h, em)
			} else {
				autoContinue = continueTurn(ctx, h, em)
				continue
			}
		}
		if users == nil { // input is done; finish the background work, then leave
			if !drainStep(ctx, h, em) {
				em.emit(&proto.Exit{Reason: "input closed"})
				return
			}
			continue
		}
		select {
		case <-h.Gate().Held():
			// taken at the top of the loop
		case msg, ok := <-users:
			if !ok {
				if in.stopped() {
					em.emit(&proto.Exit{Reason: "stopped"})
					return // explicit stop: don't wait for background jobs
				}
				// A closed channel is always ready in select, so stop selecting
				// on it and hand the loop to drainStep.
				users = nil
				continue
			}
			turn(ctx, h, em, msg.content, msg.images)
			autoContinue = true
		case <-h.Wake():
			// A background job finished; run a turn to process its notification.
			if !mayWake() {
				holdWakes(h, em)
				continue
			}
			autoContinue = continueTurn(ctx, h, em)
		case <-wakeHoldChanged:
			// Held: what is pending now is kept (the top of the loop). The
			// hold ended without a restart: what it kept runs now.
			autoContinue = true
		case <-ctx.Done():
			// Only ctrl-C reaches here now: --timeout bounds a turn, not the
			// session. Say so, rather than dying silently and leaving the TUI to
			// report a bare "engine exited".
			em.emit(&proto.Exit{Reason: "interrupted"})
			return
		}
	}
}

// drainStep advances the post-input drain by one turn, returning false once
// there is nothing left to wait for.
//
// It only ever BLOCKS while a job is actually in flight. With nothing running it
// takes a queued wake (or a leftover notification) and finishes, so a pending
// entry that arrived without a wake — proactive RAG publishes straight to the
// inbox — can't strand the loop until --timeout.
func drainStep(ctx context.Context, h *harness.Harness, em *emitter) bool {
	// A failed turn ends the drain rather than being retried. Unlike the
	// interactive loop there is nobody left to fix anything — stdin is closed —
	// so the pending work would fail identically forever.
	if h.BackgroundRunning() > 0 {
		if h.AllJobsQuiet(bareQuietJobEnd) {
			return false
		}
		select {
		case <-h.Wake():
		case <-ctx.Done():
			return false
		case <-time.After(h.QuietIn(bareQuietJobEnd)):
			if h.AllJobsQuiet(bareQuietJobEnd) {
				return false
			}
		}
		return continueTurn(ctx, h, em)
	}
	select {
	case <-h.Wake(): // a completion that raced the BackgroundRunning check
		return continueTurn(ctx, h, em)
	default:
	}
	if h.Pending() > 0 {
		return continueTurn(ctx, h, em)
	}
	return false
}

// continueTurn runs a turn with no new user message (to process a background
// job's completion notification, or a message buffered mid-turn) and emits its
// events.
func continueTurn(ctx context.Context, h *harness.Harness, em *emitter) bool {
	em.emit(&proto.Turn{Kind: "continue"})
	h.BeginTurnStats()
	tctx, end := beginTurn(ctx)
	defer end()
	h.Gate().Begin()
	res, err := h.Continue(tctx)
	h.Gate().End()
	if errors.Is(err, harness.ErrTurnHeld) {
		// No ask open, the hour's automatic turns used: the news is a note
		// (turn_state's notes_waiting), and no turn ran.
		em.emit(&proto.Done{Reason: "held"})
		return true
	}
	// turnErr first: a budget cut arrives as an interrupt, and is an error.
	err = turnErr(tctx, err)
	if errors.Is(err, harness.ErrInterrupted) {
		emitInterrupted(h, em, res)
		return true // what was queued during it runs next
	}
	if err != nil {
		emitFailedUsage(h, em, res)
		emitTurnError(ctx, h, em, err)
		return false
	}
	if strings.TrimSpace(res.Reply) != "" {
		em.emit(&proto.Message{Role: "assistant", Content: res.Reply, ID: replyID(h, res.Reply)})
	}
	em.emit(usageEvent(h, res.Usage))
	em.emit(&proto.Done{})
	return true
}

// suggestEnabled mirrors --no-suggest (inverted); the `suggest` control command emits next-message
// suggestions after `done` when set. On by default.
var suggestEnabled bool

// engineModel, engineURL and engineProvider are what this engine runs on, for
// the settings event. Set once at start.
var engineModel, engineURL, engineProvider string

// settingsEvent is the session's shared toggles.
func settingsEvent() *proto.Settings {
	ev := &proto.Settings{Model: engineModel, Provider: engineProvider, URL: engineURL,
		Rephrase: enginecmd.RephraseOn, Suggest: enginecmd.SuggestMode, Effort: enginecmd.EffortLevel}
	ev.EffortWords = enginecmd.EffortWords(enginecmd.EffortLevel)
	var roles []harness.RoleChoice
	h, _ := enginecmd.EngineHarness.Load().(*harness.Harness)
	if h != nil {
		roles = h.Roles()
	}
	ev.Tiers, ev.DefaultTier = tiersProto(engineTiers, roles), engineTiers.Default
	if h != nil {
		max, from, buf, lod, nudge, ceil, ttl := h.ContextSettings()
		ev.Context = &proto.ContextSettings{Max: max, MaxFrom: from, CompactBuffer: buf, ToolLOD: lod, RecapNudge: nudge, Ceiling: ceil, CacheTTL: ttl}
	}
	return ev
}

// contextConfigCmd is /context config's engine side: "" the settings in
// force; "--max 256k --compact-buffer 32k --tool-lod 4k --recap-nudge 20k",
// any of them, each a number of tokens (characters for --tool-lod), or
// default.
func contextConfigCmd(ctx context.Context, h *harness.Harness, action string) (string, bool, error) {
	f := strings.Fields(action)
	if len(f) == 0 {
		return h.ContextStatus(), false, nil
	}
	changes := map[string]int{}
	for i := 0; i < len(f); i++ {
		k, v, hasEq := strings.Cut(strings.TrimLeft(f[i], "-"), "=")
		if !strings.HasPrefix(f[i], "--") || !slices.Contains(harness.ContextKeys, k) {
			return "", false, fmt.Errorf("%q: want --%s", f[i], strings.Join(harness.ContextKeys, ", --"))
		}
		if !hasEq {
			if i+1 >= len(f) {
				return "", false, fmt.Errorf("--%s needs a value (a number like 128k, or default)", k)
			}
			i++
			v = f[i]
		}
		if v == "default" || v == "auto" {
			changes[k] = 0
			continue
		}
		n, err := harness.ParseTokens(v)
		if err != nil {
			return "", false, fmt.Errorf("--%s: %v", k, err)
		}
		changes[k] = n
	}
	msg, err := h.ConfigureContext(ctx, changes)
	return msg, true, err
}

// turnTimeout is --timeout applied PER TURN (interactive engine only; 0 = none).
// See the comment where it is set.
var turnTimeout time.Duration

// A turn that hangs is cut off without taking the session with it — the next
// message starts a fresh turn against a live context. See beginTurn.

// wakeHold is dund's proto.UpgradeHold: while on, the engine starts no turn
// of its own (a wake, or news left pending after a turn); it writes the news
// into the conversation and reports wake_held, and dund wakes the session
// after its restart (USER 2026-11-17). A person's message is not held.
// wakePass lets one wake through: dund's waking ServiceNotice, its one wake
// while an upgrade is pending. wakeHoldChanged pokes the loop.
var (
	wakeHold        atomic.Bool
	wakePass        atomic.Bool
	wakeHoldChanged = make(chan struct{}, 1)
	wakeHoldGen     atomic.Int64 // the newest hold taken; older ones are ignored
)

// setWakeHold takes dund's hold of generation gen, unless a newer one was
// taken (0: an unnumbered one, always taken). One caller, the input reader.
func setWakeHold(on bool, gen int64) {
	if gen != 0 {
		if gen < wakeHoldGen.Load() {
			return
		}
		wakeHoldGen.Store(gen)
	}
	wakeHold.Store(on)
	wakePass.Store(false)
	select {
	case wakeHoldChanged <- struct{}{}:
	default:
	}
}

// mayWake reports whether a turn of the engine's own may start now, using up
// the pass if that is what lets it.
func mayWake() bool { return !wakeHold.Load() || wakePass.Swap(false) }

// holdWakes keeps the pending news for after dund's restart: into the
// conversation, and dund is told.
func holdWakes(h *harness.Harness, em *emitter) {
	if n := h.HoldPending(); n > 0 {
		em.emit(&proto.WakeHeld{Count: n})
	}
}

// turn runs one user message to completion. It reports whether the turn
// succeeded — NOT whether the engine should stop. A failed turn never ends the
// session: the conversation is on disk and the next message picks up from it.
func turn(ctx context.Context, h *harness.Harness, em *emitter, task string, imgs []proto.Image) bool {
	// Known to be refused (no vision, nothing to describe the images): the
	// message goes back to the user before it is shown as sent.
	if len(imgs) > 0 {
		if why := h.ImagesRefused(); why != "" {
			refuseImages(em, why, task, imgs)
			return true
		}
	}
	// What the user typed, before any rephrase: every client renders the
	// message from this, not from what it sent itself.
	mid := harness.NewMessageID()
	em.emit(&proto.UserEcho{ID: mid, Content: task})
	em.emit(&proto.Turn{Kind: "user"})
	h.BeginTurnStats()
	tctx, end := beginTurn(ctx)
	defer end()
	// While the turn runs the input router queues a message for it
	// (harness.TurnGate) instead of handing it to the loop as a new one.
	h.Gate().Begin()
	if len(imgs) > 0 {
		// Not rephrased: a rewrite would move or drop the labels the images
		// are placed by.
		res, err := h.AskImages(tctx, task, dunImages(imgs))
		h.Gate().End()
		var refused *harness.ImageRefusedError
		if errors.As(err, &refused) {
			refuseImages(em, refused.Msg, task, imgs)
			return true
		}
		return endTurn(ctx, tctx, h, em, res, err)
	}
	if enginecmd.RephraseOn {
		// Rephrase BEFORE Ask: the store publishes the message it is passed, so
		// the rephrased text is what the session records and the model acts on.
		// The TUI still shows what the user typed. Best-effort — a failure
		// falls back to the original (see Rephrase).
		if r, err := h.Rephrase(tctx, task); err == nil {
			task = r
		}
	}
	res, err := h.AskAs(tctx, mid, task)
	h.Gate().End()
	return endTurn(ctx, tctx, h, em, res, err)
}

// endTurn reports how a user turn ended (see turn).
func endTurn(ctx, tctx context.Context, h *harness.Harness, em *emitter, res agent.TurnResult, err error) bool {
	if errors.Is(err, harness.ErrTurnHeld) { // an automatic message, held (continueTurn)
		em.emit(&proto.Done{Reason: "held"})
		return true
	}
	// turnErr first: a budget cut arrives as an interrupt, and is an error.
	err = turnErr(tctx, err)
	if errors.Is(err, harness.ErrInterrupted) {
		emitInterrupted(h, em, res)
		return true
	}
	if err != nil {
		emitFailedUsage(h, em, res)
		emitTurnError(ctx, h, em, err)
		return false
	}
	em.emit(&proto.Message{Role: "assistant", Content: res.Reply, ID: replyID(h, res.Reply)})
	em.emit(usageEvent(h, res.Usage))
	em.emit(&proto.Done{})
	return true
}

// replyID is the id of the turn's reply entry (the message event's id), or
// "" for a turn with no reply text.
func replyID(h *harness.Harness, reply string) string {
	if strings.TrimSpace(reply) == "" {
		return ""
	}
	return h.ReplyID()
}

// emitInterrupted ends a turn the user stopped. Not an error: the transcript
// holds what the turn produced and a marker, and the next message continues
// from there. The partial reply has already streamed as tokens, so there is
// no message event; done's reason tells clients to close the turn as cut.
func emitInterrupted(h *harness.Harness, em *emitter, res agent.TurnResult) {
	em.emit(usageEvent(h, res.Usage))
	em.emit(&proto.Done{Reason: "interrupted", Detail: h.InterruptedJobs()})
}

// emitFailedUsage reports what a failed turn cost. A turn that ends on an
// error (max turns, a provider failure) has still spent its rounds: without
// this its turn_stats and spend are in no usage event.
func emitFailedUsage(h *harness.Harness, em *emitter, res agent.TurnResult) {
	if res.Usage.Turns > 0 {
		em.emit(usageEvent(h, res.Usage))
	}
}

// emitTurnError reports a failed turn, and says whether the SESSION is over.
//
// Those are different facts and conflating them is what made a 30-minute
// deadline look like a crash: fatal is true only when the session context
// itself is gone (ctrl-C), in which case no message can help. Otherwise the
// turn is what died — the conversation is on disk, and the next message pairs
// off whatever was interrupted and continues from there.
//
// It also RECORDS the error in the session log, before emitting it. The UI is
// a scrollback: it shows the error once, to whoever is watching at that second,
// and then the failure is gone — which left the log with a turn that stopped
// mid-tool-call and no statement of why. Both turn paths funnel through here so
// the record cannot be attached to one of them and not the other; they had
// already drifted apart once.
func emitTurnError(sessionCtx context.Context, h *harness.Harness, em *emitter, err error) {
	h.RecordTurnError(err)
	if e := dunconfig.SetupMissing(err); e != nil {
		err = e
	}
	em.emit(&proto.Error{Error: err.Error(), Fatal: sessionCtx.Err() != nil})
}

// emitSuggestions asks for likely next user messages and emits them
// (best-effort). No-op unless suggestions are enabled.
//
// Called ONLY from the `suggest` control command now. It used to run at the end
// of every turn, which put a full LLM call between a tool result and the next
// thing the model did, and fired on autonomous turns nobody asked for. The UI
// decides when to ask, because the trigger depends on facts the engine cannot
// see: whether the input box is empty, and how long the person has sat still.
func emitSuggestions(ctx context.Context, h *harness.Harness, em *emitter) {
	if !suggestEnabled {
		return
	}
	sugs, err := h.IdleSuggestions(ctx) // once per idle, for every client (suggest.go)
	if err != nil || len(sugs) == 0 {
		return
	}
	items := make([]proto.Suggestion, len(sugs))
	for i, s := range sugs {
		items[i] = proto.Suggestion{Text: s.Text, Prob: s.Prob}
	}
	em.emit(&proto.Suggestions{Items: items})
}

// closeSession discards this session for good: its worktree, its branch, and
// its transcript, so /resume cannot offer it back.
//
// Deliberately destructive and deliberately explicit — it is the counterpart to
// the startup prune, which refuses to delete anything holding work. This is how
// a human says "that work is finished with", which is the only way that call
// can safely be made.
func closeSession(workspace, sessionID string, wt *harness.Worktree, rag harness.RagLink) string {
	var parts []string
	if wt != nil && wt.Branch != "" {
		root := harness.RepoRoot(workspace)
		if err := harness.RemoveWorktree(root, wt.Path, wt.Branch); err != nil {
			parts = append(parts, "worktree NOT removed: "+err.Error())
		} else {
			parts = append(parts, "removed worktree "+wt.Branch)
		}
	}
	if sessionID != "" {
		// Its folded history's documents first: ForgetSession removes the
		// files that name them.
		if err := harness.ForgetSessionFolds(rag, workspace, sessionID); err != nil {
			parts = append(parts, "folded history not yet removed from dund: "+err.Error())
		}
		harness.ForgetSession(workspace, sessionID)
		parts = append(parts, "forgot session "+sessionID)
	}
	if len(parts) == 0 {
		return "nothing to close — this session has no worktree and no transcript"
	}
	return "closed: " + strings.Join(parts, "; ")
}

// reportHousekeeping says what was cleaned and, more importantly, what was not.
// An abandoned worktree is the one thing here a human has to decide about.
func reportHousekeeping(res harness.PruneResult, sessions []string) {
	if n := len(res.Pruned); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: pruned %s (%s)\n", textfmt.Plural(n, "stale worktree"), numfmt.HumanBytes(res.Freed))
	}
	if n := len(sessions); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: pruned %s\n", textfmt.Plural(n, "old session"))
	}
	if n := len(res.Abandoned); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: %s still hold work —\n", textfmt.Plural(n, "abandoned worktree"))
		for _, w := range res.Abandoned {
			fmt.Fprintf(os.Stderr, "  %s\n", w.Summary())
		}
		fmt.Fprintf(os.Stderr, "  (resume one with --resume, or discard it from the TUI with /close)\n")
	}
}
