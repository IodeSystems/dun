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
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/mcptable"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// Tool servers the user can turn on and off: /rag (the docs index) and
// /lsp (semantic code). Neither starts on its own — see
// dun.DefaultServers for why — so this is how a session gets them, and how a
// machine that wants them every time says so once.
//
// The command is an extension's slash command (its manifest's slash.name):
// /rag switches the server raglit, /lsp the server poly-lsp.

// flagOf is the --rag / --lsp flag that starts a server for one run.
func flagOf(id string) string {
	switch id {
	case dun.ServerDocs:
		return "rag"
	case dun.ServerCode:
		return "lsp"
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

// noAutostart applies --no-rag / --no-lsp: the same as --rag=false /
// --lsp=false, so the two spellings cannot drift. Both together with the
// positive flag (--rag --no-rag) is an error, not a guess.
func noAutostart(t *tristate, no bool, name string) error {
	if !no {
		return nil
	}
	if t.set && t.val {
		return fmt.Errorf("--%s and --no-%s contradict", name, name)
	}
	t.set, t.val = true, false
	return nil
}

// runServerCmd applies one /rag or /lsp action and returns the line to show the
// user. A failure to start is a RETURNED MESSAGE, never a fatal error: a
// misconfigured raglit should cost you search, not your session.
//
// Actions: "" or "status" (report), "on" and "off" (a switch: this session's
// unless a scope flag follows, "on --project"), "unset --project|--session",
// "restart", "auto" (the project switch on) and "manual" (the project switch
// off; it was "stop starting it here", which off at project scope is).
func runServerCmd(ctx context.Context, h *dun.Harness, alias, action string) string {
	if alias == enginecmd.AllServers {
		return runAllServersCmd(ctx, h, action)
	}
	if verb, dir, _ := strings.Cut(strings.TrimSpace(action), " "); enginecmd.ExcludeVerb(alias, verb) {
		return runExcludeCmd(ctx, h, alias, verb, dir)
	}
	if f := strings.Fields(action); alias == dun.ServerCode && lspLanguageCmd(f) {
		msg, err := switchOp(h, "lsp", f)
		if err != nil {
			return "lsp: " + err.Error()
		}
		return msg
	}
	if f := strings.Fields(action); len(f) > 1 || (len(f) == 1 && (f[0] == "unset")) {
		return runSwitchCmd(ctx, h, alias, f[0], f[1:])
	}
	id, label := alias, alias // addressed by its id
	// The TUI opens /rag setup itself (ragsetup.go); another client
	// reaches here.
	if action == "setup" && alias == dun.ServerDocs {
		return label + ": setup is a form in dun's terminal UI — /" + alias + " setup there"
	}
	switch action {
	case "", "status":
		return serverStatusLine(h, id, label)
	case "on", "off":
		return runSwitchCmd(ctx, h, alias, action, nil)
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
		t0 := time.Now()
		if err := h.StartServer(ctx, id); err != nil {
			return label + ": did not come back — " + textfmt.OneLine(err.Error()) +
				"\nit is STOPPED now; /mcp restart " + id + " to retry"
		}
		elapsed := time.Since(t0)
		if !wasRunning {
			msg := label + ": started (it was not running) · " + runningSummary(h, id)
			if elapsed > time.Second {
				msg += fmt.Sprintf(" · %s", elapsed.Round(time.Second))
			}
			return msg + ingestLine(h, id)
		}
		msg := label + ": restarted · " + runningSummary(h, id)
		if elapsed > time.Second {
			msg += fmt.Sprintf(" · %s", elapsed.Round(time.Second))
		}
		return msg + ingestLine(h, id)
	case "login", "logout":
		// /rag login: dun's raglit token (dun/raglitauth.go); the same as
		// /mcp login <id> for an OAuth server.
		return runMCPCmd(ctx, h, action+" "+id)
	case "auto":
		return runSwitchCmd(ctx, h, alias, "on", []string{"--project"})
	case "manual":
		return runSwitchCmd(ctx, h, alias, "off", []string{"--project"})
	default:
		return "unknown action " + strconv.Quote(action) + " — try /" + alias + " [on|off|unset|restart|auto|manual] [--global|--project|--session]"
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
		return "no servers configured\n" + mcpAddHint
	}
	switch action {
	case "", "status", "list":
		return serverListing(states) + "\n" + mcpAddHint
	}

	var lines, skipped []string
	for _, st := range states {
		if !st.Running {
			skipped = append(skipped, st.ID)
			continue
		}
		lines = append(lines, runServerCmd(ctx, h, st.ID, action))
	}
	if len(lines) == 0 {
		return "nothing running to " + action + "\n" + serverListing(states)
	}
	if len(skipped) > 0 {
		lines = append(lines, "skipped (not running): "+strings.Join(skipped, ", "))
	}
	return strings.Join(lines, "\n")
}

// runMCPCmd handles the engine side of /mcp add, move, refresh, remove,
// login and logout.
// add's argument is the server spec as JSON (the TUI's form builds it).
func runMCPCmd(ctx context.Context, h *dun.Harness, action string) string {
	verb, arg, _ := strings.Cut(action, " ")
	switch verb {
	case "add":
		var spec dun.ServerSpec
		if err := json.Unmarshal([]byte(arg), &spec); err != nil {
			return "mcp add: " + err.Error()
		}
		t0 := time.Now()
		path, err := h.AddServer(ctx, spec)
		if path == "" {
			return "mcp add: " + err.Error()
		}
		if err != nil {
			return spec.ID + ": saved to " + path + ", but did not start — " + textfmt.OneLine(err.Error()) +
				"\nfix it and /mcp add again (same id replaces), or /mcp remove " + spec.ID
		}
		// The news first — it runs, with this many tools. Where it was saved
		// the form said before it was sent.
		msg := spec.ID + " added · " + runningSummary(h, spec.ID)
		if elapsed := time.Since(t0); elapsed > time.Second {
			msg += fmt.Sprintf(" · %s", elapsed.Round(time.Second))
		}
		if st, _ := stateOf(h, spec.ID); st.Via == dun.ViaShell {
			return msg + "\n" + shellPlacementNote
		}
		// No docs to index is nothing to say here (/mcp refresh still says it).
		if docs := mcpDocsLine(ctx, h, spec.ID); docs != dun.NoResources {
			msg += "\n" + docs
		}
		return msg
	case "mode", "via", "move":
		return runViaCmd(ctx, h, strings.Fields(arg))
	case "instance":
		return runInstanceCmd(ctx, h, strings.Fields(arg))
	case "refresh":
		if arg == "" {
			return "usage: /mcp refresh <id> — re-read its docs into the index"
		}
		return arg + ": " + mcpDocsLine(ctx, h, arg)
	case "login":
		if arg == "" {
			return "usage: /mcp login <id> — log in to an oauth server in the browser"
		}
		msg, err := h.LoginServer(ctx, arg)
		if err != nil {
			return arg + ": login failed — " + textfmt.OneLine(err.Error())
		}
		return arg + ": " + msg + " · " + runningSummary(h, arg)
	case "logout":
		if arg == "" {
			return "usage: /mcp logout <id> — delete (and revoke) its stored token"
		}
		msg, err := h.LogoutServer(ctx, arg)
		if err != nil {
			return "mcp logout: " + err.Error()
		}
		return arg + ": " + msg
	case "remove", "rm":
		cleared, err := h.RemoveServer(arg)
		if err != nil {
			return "mcp remove: " + err.Error()
		}
		msg := arg + ": stopped and removed from " + dun.UserServersFile()
		if len(cleared) > 0 {
			msg += "\ncleared its switches in " + strings.Join(cleared, ", ")
		}
		return msg + "\nother projects' state files are not touched; their entries for " + arg + " are skipped when loaded"
	}
	return "unknown /mcp action " + strconv.Quote(verb)
}

// mcpToolsEvent answers control "mcptools": "" is every running server's
// cost, one line each; "<id>" that server's tools; "toggle <id> <tool>
// on|off" turns one on or off and answers with the new list.
func mcpToolsEvent(h *dun.Harness, action string) *proto.McpTools {
	f := strings.Fields(action)
	ev := &proto.McpTools{Tools: []proto.McpTool{}}
	switch {
	case len(f) == 0:
		ev.Message = toolCostListing(h.Servers())
		return ev
	case f[0] == "toggle" && (len(f) == 4 || len(f) == 5):
		ev.Server = f[1]
		ev.Message = toolSwitchLine(h, f[1], f[2], f[3], f[4:])
	case len(f) == 1:
		ev.Server = f[0]
	default:
		ev.Message = "usage: /mcp tools [id] · /mcp toggle <id> <tool> on|off|unset [--global|--project|--session]"
		return ev
	}
	tools, err := h.ServerTools(ev.Server)
	if err != nil {
		ev.Message = strings.TrimPrefix(ev.Message+"\n", "\n") + ev.Server + ": " + err.Error()
		return ev
	}
	for _, t := range tools {
		ev.Tools = append(ev.Tools, proto.McpTool{Name: t.Name, Description: t.Description, Schema: t.Schema,
			Tokens: t.Tokens, Enabled: t.Enabled, HiddenBy: t.HiddenBy, Global: t.Global, Project: t.Project, Session: t.Session, From: t.From})
	}
	return ev
}

// toolCostListing is bare /mcp tools: what each running server's schemas cost.
func toolCostListing(states []dun.ServerState) string {
	var b strings.Builder
	b.WriteString("mcp tool cost per request")
	for _, st := range states {
		if !st.Running {
			continue
		}
		b.WriteString("\n  " + st.ID + ": " + mcptable.ToolCost(st))
		if hint := heavyHint(st); hint != "" {
			b.WriteString("\n    " + hint)
		}
	}
	return b.String() + "\n/mcp tools <id> lists one server's tools, to turn some off"
}

// heavyHint names the two ways to make a heavy server cheaper.
func heavyHint(st dun.ServerState) string {
	if st.Cost < dun.HeavyServerTokens || st.Via == dun.ViaShell {
		return ""
	}
	hint := "heavy: /mcp tools " + st.ID + " to hide some"
	if st.Via == dun.ViaDirect {
		hint += ", or /mcp mode " + st.ID + " scripted"
	}
	return hint
}

// mcpDocsLine syncs a server's documentation into the docs index and says
// what happened.
func mcpDocsLine(ctx context.Context, h *dun.Harness, id string) string {
	msg, err := h.SyncMCPDocs(ctx, id)
	if err != nil {
		return "docs: not indexed — " + textfmt.OneLine(err.Error())
	}
	return msg
}

// memoriesListing renders /memories: each note's question (or its first
// line), when it was written, and what has happened to it since.
func memoriesListing(ms []dun.MemoryInfo, on bool) string {
	if !on {
		return "memories: the docs index is off — /rag on (notes are kept on disk either way)"
	}
	if len(ms) == 0 {
		return "memories: none for this project yet — /remember, or say \"remember, …\""
	}
	var b strings.Builder
	live, void := 0, 0
	for _, m := range ms {
		if m.Void {
			void++
		} else {
			live++
		}
	}
	fmt.Fprintf(&b, "memories — %d current, %d void", live, void)
	for _, m := range ms {
		title := m.Question
		if title == "" {
			title, _, _ = strings.Cut(m.Body, "\n")
		}
		if len(title) > 100 {
			title = title[:100] + "…"
		}
		var tags []string
		tags = append(tags, m.Date)
		if m.Refined > 0 {
			tags = append(tags, fmt.Sprintf("refined %d×", m.Refined))
		}
		if m.Confirmed != "" {
			tags = append(tags, "confirmed "+m.Confirmed)
		} else if m.StaleConfirm {
			tags = append(tags, "confirmed before a later change")
		}
		if m.Void {
			tags = append(tags, "void: "+textfmt.OneLine(m.VoidWhy))
		}
		fmt.Fprintf(&b, "\n  %s  · %s", title, strings.Join(tags, " · "))
	}
	b.WriteString("\nto fix one, tell the model (\"that note about X is wrong, it's Y\") — it refines or voids it with you as the source")
	return b.String()
}

// catalogEvent tells the UI which skills and MCP prompts are in reach, so
// /skill can list them and each prompt can be a slash command.
func catalogEvent(h *dun.Harness) *proto.Catalog {
	c := &proto.Catalog{Skills: []proto.Skill{}, Prompts: []proto.Prompt{}}
	for _, s := range h.Skills() {
		c.Skills = append(c.Skills, proto.Skill{Name: s.Name, Description: s.Description, Source: s.Source, UserOnly: s.UserOnly})
	}
	for _, p := range h.Prompts() {
		pp := proto.Prompt{Server: p.Server, Name: p.Name, Description: p.Description}
		for _, a := range p.Arguments {
			pp.Arguments = append(pp.Arguments, proto.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
		}
		c.Prompts = append(c.Prompts, pp)
	}
	return c
}

// serverListing renders every server and its state, one per line.
// mcpAddHint ends a bare /mcp: how to add one is otherwise invisible.
const mcpAddHint = "add one: /mcp add — a form: an id, then a URL (https://…) or a command (e.g. npx some-mcp), then a bearer token or an oauth client for a URL"

func serverListing(states []dun.ServerState) string {
	var b strings.Builder
	b.WriteString("mcp servers — " + mcptable.SwitchLegend)
	b.WriteString("\n" + mcptable.ServerTable(states, mcptable.PlainKey))
	total, direct := 0, 0
	for _, st := range states {
		if st.Running && st.Via != dun.ViaShell {
			total += st.Cost
			direct++
		}
		if hint := heavyHint(st); st.Running && hint != "" {
			b.WriteString("\n  " + st.ID + ": " + hint)
		}
	}
	if direct > 0 {
		b.WriteString("\n  tool definitions: ~" + numfmt.KTokens(total) + " tokens in every request, from " +
			textfmt.Plural(direct, "running server") + " (/context shows the whole prompt)")
	}
	b.WriteString("\n  /mcp on|off <id> [--global|--project|--session] (bare: this session) · /mcp unset <id> --project|--session · /mcp mode <id> standalone|scripted")
	return b.String()
}

// serverStatusLine is the bare "/rag" report: what it is doing now, what it
// will do next session, and the one command that changes each.
func serverStatusLine(h *dun.Harness, id, label string) string {
	st, ok := stateOf(h, id)
	if !ok {
		return label + ": not configured"
	}
	cmd := id
	if s := enginecmd.SlashOf(st); s != "" {
		cmd = s
	}
	var b strings.Builder
	b.WriteString(label + ": ")
	if st.Starting {
		fmt.Fprintf(&b, "starting · %ds", int(st.StartingSeconds+0.5))
	} else if st.Running {
		b.WriteString("running · " + textfmt.Plural(st.Tools, "tool"))
	} else {
		b.WriteString("stopped")
	}
	b.WriteString(" · " + mcptable.StateCell(st) + " · switches: global " + mcptable.CellWord(st.Global) +
		", project " + mcptable.CellWord(st.Project) + ", session " + mcptable.CellWord(st.Session))
	if st.Err != "" {
		b.WriteString("\nlast start failed: " + textfmt.OneLine(st.Err))
	}
	if st.Running && st.Ingest != "" {
		b.WriteString("\n" + st.Ingest)
	}
	if ix := indexStatus(st, cmd); ix != "" {
		b.WriteString("\n" + ix)
	}
	switch {
	case !st.Running || st.Setup == nil:
	case len(st.Setup.Capabilities) > 0:
		b.WriteString("\n" + dun.CapabilityTable(*st.Setup, cmd))
	default:
		b.WriteString("\n" + dun.SetupTable(*st.Setup))
	}
	if st.Auto {
		b.WriteString("\n/" + cmd + " off to stop it this session · /" + cmd + " manual to keep it off in this project")
	} else {
		b.WriteString("\n/" + cmd + " on to start it this session · /" + cmd + " auto to start it in this project every session")
	}
	for _, e := range h.Extensions() {
		if e.ID != id || e.Slash == nil {
			continue
		}
		if own := enginecmd.ExtOwnCommands(e); len(own) > 0 {
			b.WriteString("\ncommands:")
			for i := 0; i+1 < len(own); i += 2 {
				fmt.Fprintf(&b, "\n  /%s %-12s %s", cmd, own[i], own[i+1])
			}
		}
	}
	return b.String()
}

// ingestLine is the docs server's ingest summary as a line of its own — what
// was indexed, what was skipped and how to include it — or "".
func ingestLine(h *dun.Harness, id string) string {
	if st, ok := stateOf(h, id); ok && st.Ingest != "" {
		return "\n" + st.Ingest
	}
	return ""
}

func runningSummary(h *dun.Harness, id string) string {
	st, ok := stateOf(h, id)
	if ok && st.Via == dun.ViaShell {
		return mcptable.HostedSummary(st)
	}
	if ok && st.Starting {
		return "starting… (its tools join when it is up)"
	}
	if !ok || !st.Running {
		return "not running"
	}
	return "running · " + textfmt.Plural(st.Tools, "tool")
}

// shellPlacementNote is what a server gives up in the shell; said where the
// choice is made.
const shellPlacementNote = "scripted, its schemas leave every request, but dun no longer indexes its docs (/mcp refresh), " +
	"lists its prompts or skills, applies its tool deny-list, or renders its calls — they show as eval"

func stateOf(h *dun.Harness, id string) (dun.ServerState, bool) {
	for _, st := range append(h.Servers(), h.BuiltinGroups()...) {
		if st.ID == id {
			return st, true
		}
	}
	return dun.ServerState{}, false
}

// ctrlCmdAsks reports whether a control command may put a question to the human
// before it finishes. Those must not run on the reader goroutine — see the
// caller in engine.go. Kept beside the commands themselves so adding an asking
// one is a change in this file, not a hang found in production.
func ctrlCmdAsks(id, action string) bool {
	return id == "worktree" && action == "commit" || isolateBuilds(id, action)
}

// runControlCmd handles /isolate, its old words /docker and /worktree new,
// and /worktree's own status and commit.
func runControlCmd(ctx context.Context, h *dun.Harness, id, action string) string {
	if iso, note, ok := isolateAlias(id, action); ok {
		return note + "\n" + runIsolateCmd(h, iso)
	}
	switch id {
	case "isolate":
		return runIsolateCmd(h, action)
	case "docker":
		return "unknown action " + strconv.Quote(action) + " — /docker is /isolate now: /isolate [none|worktree|docker|off]"
	case "worktree":
		return runWorktreeCmd(ctx, h, action)
	default:
		return "unknown control target " + strconv.Quote(id) + " — try /isolate or /worktree"
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

// runWorktreeCmd handles /worktree [status|commit]. /worktree new is
// /isolate worktree (isolateAlias).
func runWorktreeCmd(ctx context.Context, h *dun.Harness, action string) string {
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
	case "commit":
		return runWorktreeCommit(ctx, h)
	default:
		return "unknown action " + strconv.Quote(action) + " — try /worktree [status|commit]; /isolate worktree makes one"
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
// Harness.AskUser and setCtrlCmd (inputstream.go).
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
			return "worktree: could not write a commit message — " + textfmt.OneLine(err.Error())
		}
		q := "Commit these changes?\n\n" + indentLines(msg, "  ") + "\n\n" + indentLines(view.Status(), "  ")
		options := []string{"commit", "regenerate", "cancel"}
		if round >= maxCommitRounds {
			options = []string{"commit", "cancel"} // out of rolls; say so by not offering it
		}
		ans, err := h.AskUser(ctx, q, options, "commit")
		if err != nil {
			return "worktree: not committed — " + textfmt.OneLine(err.Error())
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

// activityEvery is how often, at most, a server's activity sends the server
// list again (main.go's OnServerActivity).
const activityEvery = 150 * time.Millisecond

// lspLanguageCmd reports whether /lsp's words address a language rather than
// the code server itself: on|off|unset followed by a name (the server's
// switch takes a scope flag there, or nothing), or add|remove.
func lspLanguageCmd(f []string) bool {
	if len(f) == 0 {
		return false
	}
	switch f[0] {
	case "add", "remove":
		return true
	case "on", "off", "unset":
		return len(f) > 1 && !strings.HasPrefix(f[1], "-")
	}
	return false
}
