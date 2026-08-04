package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/agentkit/llm"
)

// Tool servers the user can turn on and off: /rag (raglit, the docs index) and
// /lsp (poly-lsp-mcp, semantic code). Neither starts on its own — see
// dun.DefaultServers for why — so this is how a session gets them, and how a
// machine that wants them every time says so once.
//
// The command names are what the tools are called; the server ids are the tool
// families they provide.
var serverAliases = map[string]string{
	"rag": dun.ServerDocs,
	"lsp": dun.ServerCode,
}

// serverLabel renders "rag (docs)" for messages: the command the user types,
// then the id they will see in logs and config files.
func serverLabel(alias string) string {
	return alias + " (" + serverAliases[alias] + ")"
}

// aliasOf is serverAliases reversed: the command name for a server id, or the
// id itself for a server that has no command (shell, or a project's own).
func aliasOf(id string) string {
	for alias, sid := range serverAliases {
		if sid == id {
			return alias
		}
	}
	return id
}

// tristate is a bool flag that remembers whether it was given at all, so
// "--rag=false" (force off for this run) is distinguishable from not passing
// --rag (use the saved setting).
type tristate struct {
	set bool
	val bool
}

func (t *tristate) String() string {
	if t == nil || !t.set {
		return ""
	}
	return strconv.FormatBool(t.val)
}

func (t *tristate) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("want true or false, got %q", s)
	}
	t.set, t.val = true, v
	return nil
}

// IsBoolFlag lets the flag package accept a bare --rag (as --rag=true).
func (t *tristate) IsBoolFlag() bool { return true }

// autostartOverrides turns the --rag/--lsp flags into a dun.Config override.
// Empty when neither was passed, which is what leaves the saved setting alone.
func autostartOverrides(rag, lsp tristate) map[string]bool {
	out := map[string]bool{}
	if rag.set {
		out[dun.ServerDocs] = rag.val
	}
	if lsp.set {
		out[dun.ServerCode] = lsp.val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// allServers is the id /mcp uses to address every configured server at once.
// Not a legal server id (ids come from config and from dun.DefaultServers), so
// it cannot collide with one.
const allServers = "*"

// runServerCmd applies one /rag or /lsp action and returns the line to show the
// user. A failure to start is a RETURNED MESSAGE, never a fatal error: a
// misconfigured raglit should cost you search, not your session.
//
// Actions: "" or "status" (report), "on", "off", "restart", "auto" (start +
// remember), "manual" (stop remembering, leave running).
func runServerCmd(ctx context.Context, h *dun.Harness, alias, action string) string {
	if alias == allServers {
		return runAllServersCmd(ctx, h, action)
	}
	id, ok := serverAliases[alias]
	if !ok {
		id = alias // a project-defined server, addressed by its id
	}
	label := alias
	if _, known := serverAliases[alias]; known {
		label = serverLabel(alias)
	}
	switch action {
	case "", "status":
		return serverStatusLine(h, id, label)
	case "on":
		if err := h.StartServer(ctx, id); err != nil {
			return label + ": did not start — " + oneLine(err.Error()) + "\ncarrying on without it; fix and try /" + alias + " on again"
		}
		return label + ": " + runningSummary(h, id)
	case "off":
		if err := h.StopServer(id); err != nil {
			return label + ": " + err.Error()
		}
		return label + ": stopped"
	case "restart":
		// The EXPLICIT form of what StartServer deliberately refuses to do
		// implicitly (see its comment): bounce the process and lose whatever it
		// had warmed up — raglit's index, the LSP's parse cache and child
		// fleet. That loss is the POINT when a server has gone wedged or slow;
		// it is the only way back to a clean one without ending the session.
		//
		// Naming a server is explicit intent, so a stopped one is STARTED
		// rather than refused — but the message says which happened, because
		// "restarted" and "started" mean different things about what you were
		// looking at.
		wasRunning := false
		if st, ok := stateOf(h, id); ok {
			wasRunning = st.Running
		}
		if err := h.StopServer(id); err != nil {
			return label + ": " + err.Error()
		}
		if err := h.StartServer(ctx, id); err != nil {
			return label + ": did not come back — " + oneLine(err.Error()) +
				"\nit is STOPPED now; /mcp restart " + id + " to retry"
		}
		if !wasRunning {
			return label + ": started (it was not running) · " + runningSummary(h, id)
		}
		return label + ": restarted · " + runningSummary(h, id)
	case "auto":
		path, err := h.SetAutostart(id, true)
		if err != nil {
			return label + ": " + err.Error()
		}
		msg := label + ": autostart on (saved to " + path + ")"
		if err := h.StartServer(ctx, id); err != nil {
			return msg + "\nbut it did not start now — " + oneLine(err.Error())
		}
		return msg + "\n" + label + ": " + runningSummary(h, id)
	case "manual":
		path, err := h.SetAutostart(id, false)
		if err != nil {
			return label + ": " + err.Error()
		}
		return label + ": autostart off (saved to " + path + ") — still running this session; /" + alias + " off to stop it"
	default:
		return "unknown action " + strconv.Quote(action) + " — try /" + alias + " [on|off|restart|auto|manual]"
	}
}

// runAllServersCmd is /mcp: the whole server set at once, rather than one
// server at a time the way /rag and /lsp address theirs.
//
// A bare /mcp LISTS — including the stopped ones, which is the question you
// actually have ("which of these is even up?") and which no per-server command
// answers without typing all of them.
//
// An action touches only what is RUNNING. "restart the mcp system" means bounce
// what is up; it must not silently switch on servers this session left off,
// because both are opt-in by design (see dun.DefaultServers) and turning one on
// costs a spawn plus an index build. Name it — /mcp restart <id> — to start a
// stopped one.
func runAllServersCmd(ctx context.Context, h *dun.Harness, action string) string {
	states := h.Servers()
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	if len(states) == 0 {
		return "no servers configured"
	}
	switch action {
	case "", "status", "list":
		return serverListing(states)
	}

	var lines, skipped []string
	for _, st := range states {
		if !st.Running {
			skipped = append(skipped, st.ID)
			continue
		}
		lines = append(lines, runServerCmd(ctx, h, aliasOf(st.ID), action))
	}
	if len(lines) == 0 {
		return "nothing running to " + action + "\n" + serverListing(states)
	}
	if len(skipped) > 0 {
		lines = append(lines, "skipped (not running): "+strings.Join(skipped, ", "))
	}
	return strings.Join(lines, "\n")
}

// serverListing renders every server and its state, one per line.
func serverListing(states []dun.ServerState) string {
	var b strings.Builder
	b.WriteString("mcp servers")
	for _, st := range states {
		b.WriteString("\n  " + st.ID + ": ")
		switch {
		case st.Running:
			fmt.Fprintf(&b, "running · %d tools", st.Tools)
		case st.Err != "":
			b.WriteString("stopped · last error: " + oneLine(st.Err))
		default:
			b.WriteString("stopped")
		}
		if st.Auto {
			b.WriteString(" · autostart")
		}
	}
	return b.String()
}

// serverStatusLine is the bare "/rag" report: what it is doing now, what it
// will do next session, and the one command that changes each.
func serverStatusLine(h *dun.Harness, id, label string) string {
	st, ok := stateOf(h, id)
	if !ok {
		return label + ": not configured"
	}
	alias := aliasOf(id)
	var b strings.Builder
	b.WriteString(label + ": ")
	if st.Running {
		b.WriteString(fmt.Sprintf("running · %d tools", st.Tools))
	} else {
		b.WriteString("stopped")
	}
	if st.Auto {
		b.WriteString(" · autostart on")
	} else {
		b.WriteString(" · autostart off")
	}
	if st.Err != "" {
		b.WriteString("\nlast start failed: " + oneLine(st.Err))
	}
	switch {
	case !st.Running && !st.Auto:
		b.WriteString("\n/" + alias + " on to start it now · /" + alias + " auto to start it every session")
	case !st.Running && st.Auto:
		b.WriteString("\n/" + alias + " on to start it now")
	case st.Running && !st.Auto:
		b.WriteString("\n/" + alias + " auto to start it every session · /" + alias + " off to stop it")
	default:
		b.WriteString("\n/" + alias + " manual to stop starting it automatically")
	}
	return b.String()
}

func runningSummary(h *dun.Harness, id string) string {
	st, ok := stateOf(h, id)
	if !ok || !st.Running {
		return "not running"
	}
	return fmt.Sprintf("running · %d tools", st.Tools)
}

func stateOf(h *dun.Harness, id string) (dun.ServerState, bool) {
	for _, st := range h.Servers() {
		if st.ID == id {
			return st, true
		}
	}
	return dun.ServerState{}, false
}

// serverHint is the startup line naming what is NOT running and how to get it.
// Shown once, at ready: an agent that silently lacks code navigation is the
// kind of thing a user only discovers by watching it flail with grep.
func serverHint(states []dun.ServerState) string {
	var lines []string
	for _, st := range states {
		if st.Running {
			continue
		}
		alias := aliasOf(st.ID)
		_, hasCmd := serverAliases[alias]
		switch {
		case st.Err != "":
			lines = append(lines, fmt.Sprintf("%s did not start: %s", st.ID, oneLine(st.Err)))
		case hasCmd:
			lines = append(lines, fmt.Sprintf("%s off — /%s on to start it, /%s auto to start it every session", st.ID, alias, alias))
		default:
			lines = append(lines, st.ID+" off")
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// serversToAny renders server states for a JSON event.
func serversToAny(states []dun.ServerState) []map[string]any {
	out := make([]map[string]any, 0, len(states))
	for _, st := range states {
		m := map[string]any{
			"id": st.ID, "running": st.Running, "auto": st.Auto, "tools": st.Tools,
		}
		if st.Err != "" {
			m["err"] = st.Err
		}
		out = append(out, m)
	}
	return out
}

// ctrlCmdAsks reports whether a control command may put a question to the human
// before it finishes. Those must not run on the reader goroutine — see the
// caller in main.go. Kept beside the commands themselves so adding an asking
// one is a change in this file, not a hang found in production.
func ctrlCmdAsks(id, action string) bool {
	return id == "worktree" && action == "commit"
}

// runControlCmd handles /docker, /worktree, and /ship control commands.
func runControlCmd(ctx context.Context, h *dun.Harness, id, action string) string {
	switch id {
	case "docker":
		return runDockerCmd(ctx, h, action)
	case "worktree":
		return runWorktreeCmd(ctx, h, action)
	case "ship":
		return runShipCmd(ctx, h, action)
	default:
		return "unknown control target " + strconv.Quote(id) + " — try /docker, /worktree, or /ship"
	}
}

// runDockerCmd handles /docker [on|off|status].
func runDockerCmd(ctx context.Context, h *dun.Harness, action string) string {
	switch action {
	case "", "status":
		if h.IsDocker() {
			backend := h.ExecBackend().(dun.DockerExec)
			return "docker: on (image: " + backend.Image + ")"
		}
		return "docker: off (exec runs on host)"
	case "on":
		// Rehoist: keep current workspace + worktree, switch exec to Docker.
		h.Rehoist(h.Workspace(), h.Worktree(), true)
		image := h.ExecBackend().(dun.DockerExec).Image
		return "docker: on (image: " + image + ")"
	case "off":
		// Rehoist: keep current workspace + worktree, switch exec to host.
		h.Rehoist(h.Workspace(), h.Worktree(), false)
		return "docker: off (exec runs on host)"
	default:
		return "unknown action " + strconv.Quote(action) + " — try /docker [on|off|status]"
	}
}

// gitView is the Worktree to REPORT on: the session's dedicated one, or a
// pass-through over the workspace when dun works in place.
//
// Without this, every /worktree query answered "none (working in place)" in the
// common case — worktree isolation is opt-in — which is a true statement of the
// isolation mode and no answer at all to the question being asked. Working in
// place still means working in a git repo.
func gitView(h *dun.Harness) (wt *dun.Worktree, dedicated bool) {
	if wt := h.Worktree(); wt != nil && wt.Branch != "" {
		return wt, true
	}
	inPlace, isRepo := dun.WorktreeInPlace(h.Workspace())
	if !isRepo {
		return nil, false
	}
	return inPlace, false
}

// runWorktreeCmd handles /worktree [status|new|commit].
func runWorktreeCmd(ctx context.Context, h *dun.Harness, action string) string {
	wt := h.Worktree()
	dockerOn := h.IsDocker()
	switch action {
	case "", "status":
		view, dedicated := gitView(h)
		if view == nil {
			return "worktree: " + h.Workspace() + " is not a git repository"
		}
		head := "in place: " + view.Path
		if dedicated {
			head = fmt.Sprintf("worktree: %s (base %s)", view.Path, view.BaseBranch)
		}
		return head + "\n" + view.Status()
	case "new":
		if wt != nil && wt.Branch != "" {
			return "worktree: already on branch " + wt.Branch
		}
		// Create a new worktree. NewWorktree resolves the repo root itself.
		//
		// The mounts are the session's, not nil. A worktree lives under
		// .dun/worktrees/, so a go.mod "replace => ../agentkit" resolves through
		// a symlink NewWorktree puts beside it — and passing nil here meant a
		// worktree made with /worktree new had no such symlink, so the first
		// build inside it failed on a dependency that resolves fine at startup.
		// Same list Docker mounts, so both isolation tiers see one set of paths.
		newWt, isRepo, err := dun.NewWorktree(h.Workspace(), h.Mounts())
		if err != nil {
			return "worktree: failed to create — " + oneLine(err.Error())
		}
		if !isRepo {
			return "worktree: not a git repo — cannot create worktree"
		}
		// Rehoist: move workspace + exec to the new worktree directory,
		// preserving the current docker setting.
		h.Rehoist(newWt.Path, newWt, dockerOn)
		return fmt.Sprintf("worktree: created %s (branch %s)", newWt.Path, newWt.Branch)
	case "commit":
		return runWorktreeCommit(ctx, h)
	default:
		return "unknown action " + strconv.Quote(action) + " — try /worktree [status|new|commit]"
	}
}

// maxCommitRounds bounds "regenerate". Three tries and the answer is to write it
// yourself, not to keep paying for rolls of the same dice.
const maxCommitRounds = 3

// runWorktreeCommit writes the commit message with the model, shows it, and
// commits only once the human says so.
//
// It commits IN PLACE when there is no dedicated worktree. The old version
// refused ("none — nothing to commit") which was wrong twice: it named the
// isolation mode as the reason, and the changes it declined to commit were
// sitting right there in the repo.
//
// MUST NOT run on the engine's control-command goroutine — it asks. See
// Harness.AskUser and setCtrlCmd in main.go.
func runWorktreeCommit(ctx context.Context, h *dun.Harness) string {
	view, _ := gitView(h)
	if view == nil {
		return "worktree: " + h.Workspace() + " is not a git repository"
	}
	if view.IsClean() {
		return "worktree: nothing to commit — the tree is clean"
	}
	for round := 1; ; round++ {
		msg, err := h.CommitMessage(ctx, view)
		if err != nil {
			return "worktree: could not write a commit message — " + oneLine(err.Error())
		}
		q := "Commit these changes?\n\n" + indentLines(msg, "  ") + "\n\n" + indentLines(view.Status(), "  ")
		options := []string{"commit", "regenerate", "cancel"}
		if round >= maxCommitRounds {
			options = []string{"commit", "cancel"} // out of rolls; say so by not offering it
		}
		ans, err := h.AskUser(ctx, q, options)
		if err != nil {
			return "worktree: not committed — " + oneLine(err.Error())
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "commit":
			return "worktree: " + view.Commit(msg)
		case "regenerate":
			continue
		default:
			return "worktree: cancelled — nothing was committed"
		}
	}
}

// indentLines prefixes every line, so a multi-line commit message reads as one
// quoted block in the confirmation rather than as more of the question.
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// runShipCmd handles /ship [verify|push|pr].
//
// It does NOT run ship directly. Instead it queues a forced tool call that will
// be injected into the next LLM response's tool_calls (via Harness.ForceToolCall
// + Session.OnToolCalls). The Turn loop then persists it as assistant(tool_calls)
// → tool(result) and dispatches it through withShip, making the exchange look
// like the model's own decision.
//
// If no turn is running, the caller should trigger one (e.g. by sending a
// mid-turn message or starting a Continue turn) so the injection happens.
func runShipCmd(ctx context.Context, h *dun.Harness, action string) string {
	var mode dun.ShipMode
	switch strings.ToLower(action) {
	case "", "push":
		mode = dun.ShipPush
	case "verify":
		mode = dun.ShipVerify
	case "pr":
		mode = dun.ShipPR
	default:
		return "unknown action " + strconv.Quote(action) + " — try /ship [verify|push|pr]"
	}
	args, _ := json.Marshal(map[string]string{"mode": string(mode)})
	h.ForceToolCall(llm.ToolCall{
		ID:   "call_ship_" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      "ship",
			Arguments: string(args),
		},
	})
	return "ship queued — running on next turn"
}

