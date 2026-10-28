package main

import (
	"cmp"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The slash commands the ENGINE carries out, as data (proto.Commands) and as
// one router (routeSlash). A client that is not the TUI — the web app, the
// phone — gets the list from the commands event and sends a typed line as
// control "slash"; the TUI keeps its own table for the commands only it can
// run (mouse, trace, forms, pickers), and a test pins its engine-routed
// entries to this list.

func choices(pairs ...string) []proto.CommandChoice {
	out := make([]proto.CommandChoice, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, proto.CommandChoice{Value: pairs[i], Description: pairs[i+1]})
	}
	return out
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// commandsEvent is the list, with the current state of each toggle.
func commandsEvent(h *dun.Harness) *proto.Commands {
	exts, _ := engineExtensions(h.Extensions())
	return commandList(h.Servers(), isolationMode(h), exts...)
}

// commandList builds the list from the state it shows: the extensions'
// slash commands (the bundled ones when exts is empty), after dun's own.
func commandList(servers []dun.ServerState, iso string, exts ...dun.Extension) *proto.Commands {
	if len(exts) == 0 {
		exts = dun.BundledExtensions()
	}
	serverState := func(alias string) (string, string) {
		var st dun.ServerState
		ok := false
		for _, s := range servers {
			if s.ID == alias {
				st, ok = s, true
			}
		}
		if !ok {
			return "off", "not configured"
		}
		detail := ""
		if st.Auto {
			detail = "autostart"
		}
		return onOff(st.Running), detail
	}
	var items []proto.CommandSpec
	for _, e := range exts {
		if e.Slash == nil {
			continue
		}
		state, detail := serverState(e.ID)
		items = append(items, extCommand(e, state, detail))
	}
	return &proto.Commands{Items: append(ownCommands(iso), items...)}
}

// srvActions are the server switch's actions, as an extension's slash
// command offers them.
var srvActions = []string{"on", "on for this session (--project, --global for wider)", "off", "off for this session (--project, --global for wider)",
	"unset", "clear a --project or --session switch", "restart", "restart it",
	"auto", "on for this project (= on --project)", "manual", "off for this project (= off --project)"}

// extCommand is an extension's slash command: the server switch's actions,
// then the extension's own commands.
func extCommand(e dun.Extension, state, detail string) proto.CommandSpec {
	pairs := append([]string{}, srvActions...)
	usage := "on|off|unset|restart|auto|manual"
	own := extOwnCommands(e)
	var names []string
	for i := 0; i+1 < len(own); i += 2 {
		names = append(names, own[i])
	}
	pairs = append(pairs, own...)
	if len(names) > 0 {
		usage += "|" + strings.Join(names, "|")
	}
	usage = "[" + usage + "] [--global|--project|--session]"
	if len(names) > 0 {
		usage += " | <command> [text]"
	}
	return proto.CommandSpec{Name: e.Slash.Name, Usage: usage, Group: "tools", Choices: choices(pairs...),
		Text: len(names) > 0, State: state, Detail: detail, Help: e.Slash.Help,
		Description: firstNonEmpty(e.Slash.Description, e.Description)}
}

// extOwnCommands are e's commands beside the server switch, as name, help
// pairs: the manifest's (by name), then the index commands its roles give it.
func extOwnCommands(e dun.Extension) []string {
	var pairs []string
	names := make([]string, 0, len(e.Slash.Commands))
	for n := range e.Slash.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := e.Slash.Commands[n]
		help := c.Help
		if help == "" && c.Tool != "" {
			help = "calls " + c.Tool
		}
		pairs = append(pairs, n, help)
	}
	if _, own := e.Slash.Commands["watch"]; !own && extIndexVerb(e, "watch") {
		pairs = append(pairs, "watch", "watch a directory into an index of this project: watch <dir> [--index NAME]")
	}
	if _, own := e.Slash.Commands["exclude"]; !own && excludeVerb(e.ID, "exclude") {
		pairs = append(pairs, excludeHelp...)
	}
	if _, own := e.Slash.Commands["setup"]; !own && e.ID == dun.ServerDocs {
		pairs = append(pairs, ragSetupHelp...)
	}
	if _, own := e.Slash.Commands["attach"]; !own && extIndexVerb(e, "attach") {
		pairs = append(pairs, "attach", "search another index too: attach <index>|<project>:<index> [--project|--global]; bare lists",
			"detach", "stop searching an attached index: detach <index> [--project|--global]")
	}
	if _, own := e.Slash.Commands["workspace"]; !own && extIndexVerb(e, "workspace") {
		pairs = append(pairs, "workspace", "whether this workspace is read into an index: workspace [on|off] [--session|--project|--global]; off keeps search on other indexes")
	}
	return pairs
}

// ownCommands are dun's own engine commands, which no extension's slash
// command may shadow.
func ownCommands(iso string) []proto.CommandSpec {
	// In the order a client lists them within a group: /mcp, which a new
	// user adds a server with, before /ext, which explains extensions.
	return []proto.CommandSpec{
		{Name: "mcp", Usage: "[info | help | add [preset] | tools [id] | on|off <id> [--global|--project|--session] | unset <id> --project|--session | mode <id> standalone|scripted|unset [--scope] | toggle <id> <tool> on|off|unset [--scope] | remove <id> | refresh <id> | login|logout <id> | restart [id]]", Group: "tools", Text: true,
			Choices: choices("info", "each server's state, mode, tools and tokens; the shell; what scripting a heavy one saves",
				"help", "these actions, one line each",
				"add", "add a server: a form — a preset, or an id and a URL or a local command",
				"on", "turn a server on: this session, or --project / --global", "off", "turn a server off: this session, or --project / --global",
				"unset", "clear a server's --project or --session switch", "restart", "restart servers", "remove", "remove a server from the catalog", "refresh", "re-index a server's docs",
				"mode", "standalone (its own tools, schemas in every request) or scripted (in mcpshell, called through eval), at a scope",
				"toggle", "turn one tool on or off, at a scope",
				"tools", "what each server's tools cost; with an id, list them and turn some off",
				"login", "log in to an oauth server in the browser", "logout", "delete (and revoke) an oauth server's token"),
			Description: "tool servers (MCP): see them and turn them on or off; /mcp add adds one",
			Help: "/mcp alone opens a table of every server: on or off, and where that is set (this session, this project, every project).\n" +
				"/mcp add opens a form: a name, and the command or web address you were given. A preset fills the server in from its name: " + presetIDs() + ".\n" +
				"The same name again replaces it."},
		{Name: "ext", Usage: "[info]", Group: "tools", Choices: choices("info", "the same list"),
			Description: "extensions: where each comes from, its server, commands and hooks"},
		// /docker and /worktree new are /isolate's old words: still routed
		// (routeSlash, isolateAlias), not listed.
		{Name: "isolate", Usage: "[none|worktree|docker|off]", Group: "tools", Choices: isolateChoices,
			State: cmp.Or(iso, dun.IsoNone), Description: "where dun's edits and commands are: in place, a fresh git worktree, or a worktree with commands in Docker; alone shows which",
			Help: "/isolate alone says where this session works now: in place (the checkout itself), a worktree (its path and branch), or docker (a worktree, its commands in a container; the image).\n" +
				"/isolate worktree moves the session into a fresh git worktree on a new branch: your checkout is not touched.\n" +
				"/isolate docker does the same and runs its commands in Docker on the worktree. A container on your own checkout would still change your files, so docker always brings a worktree.\n" +
				"/isolate off (or none) goes back to the checkout. The worktree is kept on disk and the reply says where: merge its branch, or remove it yourself.\n" +
				"/isolate docker image shows the image docker uses (golang:1.23 until you choose). /isolate docker image <image> sets it; a path to a Dockerfile, or to a directory holding one, is built with docker build (tag dun-iso-<hash of the Dockerfile>, not rebuilt while that tag exists) and that tag is set. Sub-agents with isolation docker use it too. Only commands run in it: dun itself stays on the host. Not kept for the next session.\n" +
				"Both need a git repository; docker needs Docker installed."},
		{Name: "worktree", Usage: "[status|commit]", Group: "git", Choices: choices("status", "", "commit", "write the message and ask"),
			Description: "git: alone shows status, commit writes the message and asks; /isolate worktree makes a worktree"},
		{Name: "memories", Group: "memory", Description: "this project's saved notes: when written, refined, confirmed or void"},
		{Name: "remember", Usage: "[text]", Group: "memory", Text: true,
			Description: "save a note for later sessions on this project (needs /rag on); alone: the model picks what to keep"},
		{Name: "rules", Usage: "[full|summary|index [--scope] | on|off|unset <file|glob> [--scope]]", Group: "session",
			Choices: choices("full", "the rules files and their imports, whole, in the system prompt",
				"summary", "the imports condensed once by the model (root files stay whole)",
				"index", "the imports in the docs index, surfaced by relevance (needs /rag on)",
				"on", "a file (or glob) back in every request", "off", "a file (or glob) out of every request, still found through the docs index",
				"unset", "a file's switch at a scope, back to the one below"),
			Description: "project rules (AGENTS.md, CLAUDE.md, their @imports): alone, each file's cost, its switch at each scope and the mode; a mode word sets the project's mode"},
		{Name: "retry", Usage: "[default|patient|stubborn | attempts N | budget <dur>|unbounded|default | turn <dur>|off|default]", Group: "session",
			Choices: choices("status", "the settings now", "default", retryPresets[0].about, "patient", retryPresets[1].about,
				"stubborn", retryPresets[2].about, "attempts", "how many times a 5xx is retried: attempts N",
				"budget", "how long one request keeps retrying: budget 15m, unbounded or default",
				"turn", "how long a turn whose stream died is re-run: turn 30m, off or default"),
			Description: "how hard dun keeps at a flaky provider: alone picks a preset; saved for every session"},
		{Name: "file", Usage: "[add|remove|off|mode|index <path> …]", Group: "tools", Choices: choices(
			"add", "a file or directory outside the workspace for dun's file tools: read only unless --write, a directory indexed unless --no-index; --global, --project or --session (the default)",
			"remove", "take one back at a scope", "off", "turn one off at a scope, keeping its settings",
			"mode", "<path> read|write at a scope", "index", "<path> on|off: whether a directory is searchable (/rag)"),
			Description: "paths outside the workspace dun may use, like /mcp's servers: alone, the table; each read or write, a directory indexed or not, at the global, project or session scope"},
		{Name: "worklog", Usage: "[start <title> | end [done|dropped|blocked] <why> | view [n] | write [n]]", Group: "session",
			Choices: choices("start", "open a task; under an open task, a subtask", "end", "close the open task: done, dropped or blocked, and why",
				"view", "one task's events: inputs, tool calls, recaps, notes", "write", "the model writes a closed task's account to a file"),
			Description: "this session's tasks: alone, the tree with times and outcomes"},
		{Name: "effort", Usage: "[default|off|low|medium|high]", Group: "session",
			Choices: choices(dun.EffortDefault, dun.EffortAbout[dun.EffortDefault], dun.EffortOff, dun.EffortAbout[dun.EffortOff],
				dun.EffortLow, dun.EffortAbout[dun.EffortLow], dun.EffortMedium, dun.EffortAbout[dun.EffortMedium],
				dun.EffortHigh, dun.EffortAbout[dun.EffortHigh]),
			State: effortLevel, Description: "how much the model thinks before it answers: alone picks a level; saved for every session"},
		{Name: "suggest", Usage: "[on|off|auto]", Group: "session", Choices: choices("on", "always", "off", "never", "auto", "when idle"),
			State: suggestMode, Description: "next-message suggestions: alone asks for some now, on/off/auto set the mode"},
		{Name: "rephrase", Usage: "[on|off|status | prompt [text|default]]", Group: "session", Choices: choices("on", "rewrite prompts first", "off", "as typed", "status", "", "prompt", "show the rephrasing prompt; prompt <text> sets it, prompt default goes back to dun's"),
			State: onOff(rephraseOn), Description: "rephrase prompts for specificity before acting"},
		{Name: "jobs", Usage: "[kill <n>|kill all]", Group: "session", Choices: choices("kill", "kill a job and everything it started: a number, or all"),
			Description: "background jobs: alone lists the running ones, kill <n> ends one and everything it started, kill all ends every one"},
		{Name: "schedule", Usage: "[<when> [--to <session>] <text> | list | push <id> <when>|+<dur> | edit <id> [text] | cancel <id>]", Group: "session", Text: true,
			Choices: choices("list", "what is scheduled, loops too", "push", "move one: push <id> 15:00, or +1h past its time",
				"edit", "replace one's text; edit <id> alone puts it in the composer", "cancel", "drop one"),
			Description: "send a message later, to this session or another: /schedule 10m <text>, 15:00, tomorrow 9:00; alone lists",
			Help: "/schedule <when> <text> sends <text> to this session at <when>, as if you typed it then; it arrives marked as scheduled.\n" +
				"<when>: 10m or 2h30m from now, 15:00 (today, or tomorrow once it has passed), tomorrow 9:00, or an RFC3339 time. Your local time.\n" +
				"--to <session> sends it to another of your sessions, by the name /sessions and dun_list give it, or its id.\n" +
				"It arrives with no client attached and after dund restarts: dund keeps it and starts the session if it stopped. A busy session reads it in its running turn.\n" +
				"/schedule lists every pending message and loop; push <id> <when> moves one (+1h: an hour past its time), edit <id> <text> replaces its text, cancel <id> drops it."},
		{Name: "loop", Usage: "[[interval] <prompt> | stop [id]]", Group: "session", Text: true,
			Choices:     choices("stop", "end this session's loop, or loop <id>"),
			Description: "run a prompt again and again: /loop 10m <prompt>; no interval, the model paces it; alone lists",
			Help: "/loop 10m <prompt> runs <prompt> now and then every 10 minutes (1m to 24h), until stopped.\n" +
				"/loop <prompt>, with no interval, lets the model pace it: while a loop runs it has a loop_ctl tool to set the next run or stop; a run that ends without it waits 20m.\n" +
				"A run waits while the session is in a turn. /loop lists the loops, /loop stop [id] ends one; /schedule push|edit|cancel work on loops too."},
		{Name: "interrupt", Group: "session", Description: "stop the running turn; what it wrote so far stays, and your next message continues from there"},
		{Name: "clear", Group: "session", Description: "clear the conversation and start a fresh session log"},
		{Name: "discard", Group: "session", Description: "discard this session for good: remove its worktree and branch, forget its transcript (it was /close; /close now exits, USER 2026-10-27)"},
	}
}

// providersMu orders provider changes and model listings in the engine, so
// the last models event a client gets reflects the last change: a listing
// started just before an edit cannot land after it with the old key.
var providersMu sync.Mutex

// modelsEvent lists every saved provider's models, fetched in parallel.
// Keys stay here; a client gets names, URLs and ids.
func modelsEvent() *proto.Models {
	cfg := loadConfig()
	ps, def := cfg.providerList(), cfg.activeName()
	out := make([]proto.ModelProvider, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, why := listModels(p.URL, p.Key)
			if ids == nil {
				ids = []string{} // a list, never null, for every client
			}
			out[i] = proto.ModelProvider{Name: p.Name, URL: p.URL, IDs: ids, Error: why,
				Default: p.Name == def, Key: p.Key != ""}
		}()
	}
	wg.Wait()
	return &proto.Models{Providers: out}
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// isJobID says s is a job number ("3" or "#3").
func isJobID(s string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
	return err == nil && n > 0
}

// jobsCmd carries out /jobs for every client: action "" lists the running
// jobs, "kill <n>" and "kill all" end them. The answer is the message shown.
func jobsCmd(h *dun.Harness, action string) string {
	f := strings.Fields(action)
	if len(f) == 0 {
		return jobsListing(h.RunningJobs(), time.Now())
	}
	if len(f) == 2 && f[0] == "kill" {
		if f[1] == "all" {
			return h.KillAllJobs("user")
		}
		if n, err := strconv.Atoi(f[1]); err == nil {
			msg, err := h.KillJob(n, "user")
			if err != nil {
				return err.Error()
			}
			return msg
		}
	}
	return "usage: /jobs [kill <n>|kill all]"
}

// jobsListing is bare /jobs: one line per running job — id, command, age,
// and its last line of output.
func jobsListing(jobs []dun.JobInfo, now time.Time) string {
	if len(jobs) == 0 {
		return "no background jobs are running"
	}
	lines := []string{fmt.Sprintf("%d running (/jobs kill <n> to stop one):", len(jobs))}
	for _, j := range jobs {
		line := "  " + dun.JobLine(j, now)
		if j.Last != "" {
			line += " ⟩ " + dun.ShortCommand(j.Last, 80)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// slashRoutes are what routeSlash needs from the engine.
type slashRoutes struct {
	server  func(alias, action string)
	control func(id, action string)
	reset   func()
	reply   func(msg string) // an answer that is not a command's own event
	// exts are the extensions whose slash commands route (engineExtensions);
	// nil is the bundled ones.
	exts []dun.Extension
}

// routeSlash carries out one slash line ("raglit on", "/remember the tests are
// slow") from a client that is not the TUI.
func routeSlash(line string, r slashRoutes) {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if len(f) == 0 {
		r.reply("an empty command")
		return
	}
	name, args := strings.ToLower(f[0]), f[1:]
	arg := func(i int) string {
		if i < len(args) {
			return strings.ToLower(args[i])
		}
		return ""
	}
	exts := r.exts
	if exts == nil {
		exts = dun.BundledExtensions()
	}
	for _, e := range exts {
		if e.SlashName() != name {
			continue
		}
		// /<slash> <command> <text> calls the extension's tool, the text as
		// typed; anything else is its server's switch.
		if _, ok := e.Slash.Commands[arg(0)]; ok && len(args) > 0 {
			r.control("ext", "call "+e.ID+" "+arg(0)+" "+strings.Join(args[1:], " "))
			return
		}
		// watch|attach|detach, by the extension's roles: a directory and an
		// index name keep their case.
		if extIndexVerb(e, arg(0)) {
			r.control("ext", strings.TrimSpace(arg(0)+" "+e.ID+" "+strings.Join(args[1:], " ")))
			return
		}
		// exclude|include <dir>: the directory keeps its case.
		if excludeVerb(e.ID, arg(0)) {
			r.server(e.ID, strings.TrimSpace(arg(0)+" "+strings.Join(args[1:], " ")))
			return
		}
		r.server(e.ID, strings.Join(lowerAll(args), " "))
		return
	}
	switch name {
	case "ext":
		if len(args) > 1 || (len(args) == 1 && arg(0) != "info") {
			r.reply("usage: /ext [info] — lists the extensions")
			return
		}
		r.control(name, "")
	case "mcp":
		switch arg(0) {
		case "add":
			r.reply("adding an MCP server takes the form in the TUI (/mcp add) for now")
		case "info":
			if len(args) > 1 {
				r.reply("usage: /mcp info")
				return
			}
			r.control("mcpinfo", "")
		case "help":
			r.reply(mcpHelp())
		case "remove", "rm", "refresh", "login", "logout":
			if len(args) != 2 {
				r.reply("usage: /mcp " + arg(0) + " <id>")
				return
			}
			r.control("mcp", arg(0)+" "+args[1])
		case "tools":
			if len(args) > 2 {
				r.reply("usage: /mcp tools [id]")
				return
			}
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			r.control("mcptools", id)
		case "mode", "via", "move":
			if _, err := dun.NormalizeVia(arg(2)); len(args) < 3 || len(args) > 4 || (err != nil && arg(2) != "unset") {
				r.reply("usage: /mcp mode <id> standalone|scripted|unset [--global|--project|--session] (bare: this session; via and move, shell and direct are the same)")
				return
			}
			r.control("mcp", "mode "+strings.Join(lowerAll(args[1:]), " "))
		case "toggle":
			if len(args) < 4 || len(args) > 5 {
				r.reply("usage: /mcp toggle <id> <tool> on|off|unset [--global|--project|--session] (bare: this session)")
				return
			}
			// The tool name keeps its case: a server's tools are case-sensitive.
			r.control("mcptools", "toggle "+arg(1)+" "+args[2]+" "+strings.Join(lowerAll(args[3:]), " "))
		case "unset":
			if len(args) != 3 {
				r.reply("usage: /mcp unset <id> --project|--session")
				return
			}
			r.server(arg(1), "unset "+arg(2))
		default:
			// An action no server knows is answered once, here: sent on, every
			// server said the same usage line (USER 2026-10-24, "/mcp inf9").
			if a := arg(0); a != "" && !serverActions[a] {
				r.reply(unknownMCPAction(a))
				return
			}
			target := allServers
			if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
				target = arg(1)
			}
			action := arg(0)
			if len(args) > 2 {
				action += " " + strings.Join(lowerAll(args[2:]), " ")
			}
			if target == allServers && len(args) > 1 {
				r.reply("usage: /mcp " + arg(0) + " <id> [--global|--project|--session] — a switch names its server")
				return
			}
			r.server(target, action)
		}
	case "rephrase":
		// Bare or status reports; only on/off change it (the engine toggle
		// reads anything else as off).
		// The engine's toggle sends no message of its own (the TUI prints
		// one), so the answer is said here.
		switch arg(0) {
		case "on", "off":
			r.control(name, arg(0))
			r.reply("rephrase: " + arg(0))
		case "prompt": // show it, set it (the rest of the line, as typed), or default
			r.control(name, strings.TrimSpace("prompt "+strings.Join(args[1:], " ")))
		default:
			r.reply("rephrase: " + onOff(rephraseOn))
		}
	case "worklog":
		// The title and the why as typed.
		switch arg(0) {
		case "", "start", "end", "view", "write":
			r.control(name, strings.TrimSpace(arg(0)+" "+strings.Join(args[min(1, len(args)):], " ")))
		default:
			r.reply("usage: /worklog [start <title> | end [done|dropped|blocked] <why> | view [n] | write [n]]")
		}
	case "effort":
		r.control(name, arg(0))
	case "file":
		// The path as typed: its case matters.
		switch {
		case len(args) == 0:
			r.control("files", "")
		case (arg(0) == "add" || arg(0) == "remove") && len(args) >= 2:
			r.control("files", arg(0)+" "+strings.Join(args[1:], " "))
		default:
			r.reply("usage: /file [add|remove <path>]")
		}
	case "retry":
		r.control(name, strings.Join(args, " "))
	case "suggest":
		r.control(name, arg(0))
		switch arg(0) {
		case "on", "off", "auto":
			r.reply("suggestions: " + arg(0))
		}
	case "isolate", "docker", "worktree": // docker, worktree new: isolateAlias
		if name == "isolate" && arg(0) == "docker" && arg(1) == "image" {
			// The image or path keeps its case and its words.
			r.control(name, strings.Join(append([]string{"docker", "image"}, args[2:]...), " "))
			break
		}
		r.control(name, arg(0))
	case "jobs":
		switch {
		case len(args) == 0 || (arg(0) == "list" && len(args) == 1):
			r.control(name, "")
		case arg(0) == "kill" && len(args) == 2 && arg(1) == "all":
			r.control(name, "kill all")
		case arg(0) == "kill" && len(args) == 2 && isJobID(args[1]):
			r.control(name, "kill "+strings.TrimPrefix(args[1], "#"))
		default:
			r.reply("usage: /jobs [kill <n>|kill all]")
		}
	case "rules":
		switch {
		case len(args) == 0:
			r.control(name, "")
		case (arg(0) == dun.RulesFull || arg(0) == dun.RulesSummary || arg(0) == dun.RulesIndex) &&
			(len(args) == 1 || len(args) == 2 && strings.HasPrefix(args[1], "--")):
			r.control(name, strings.Join(append([]string{arg(0)}, args[1:]...), " "))
		case (arg(0) == "on" || arg(0) == "off" || arg(0) == "unset" || arg(0) == "mode") && len(args) >= 2:
			r.control(name, strings.Join(append([]string{arg(0)}, args[1:]...), " "))
		default:
			r.reply("usage: /rules [full|summary|index [--scope] | on|off|unset <file|glob> [--global|--project|--session] | mode <mode|unset> [--scope]]")
		}
	case "memories", "interrupt":
		r.control(name, "")
	case "discard": // the engine's control is still "close"
		r.control("close", "")
	case "remember":
		r.control(name, strings.Join(args, " "))
	case "schedule", "loop":
		// The rest of the line as typed: a message keeps its case, spacing
		// and newlines. The engine parses it (schedule.go).
		_, rest := cutFields(strings.TrimPrefix(strings.TrimSpace(line), "/"), 1)
		r.control(name, rest)
	case "clear":
		r.reset()
	default:
		r.reply("no command /" + name)
	}
}
