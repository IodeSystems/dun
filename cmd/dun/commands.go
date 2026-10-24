package main

import (
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
	return commandList(h.Servers(), h.IsDocker(), exts...)
}

// commandList builds the list from the state it shows: the extensions'
// slash commands (the bundled ones when exts is empty), after dun's own.
func commandList(servers []dun.ServerState, docker bool, exts ...dun.Extension) *proto.Commands {
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
	return &proto.Commands{Items: append(ownCommands(docker), items...)}
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
	if _, own := e.Slash.Commands["attach"]; !own && extIndexVerb(e, "attach") {
		pairs = append(pairs, "attach", "search another index too: attach <index>|<project>:<index> [--project|--global]; bare lists",
			"detach", "stop searching an attached index: detach <index> [--project|--global]")
	}
	return pairs
}

// ownCommands are dun's own engine commands, which no extension's slash
// command may shadow.
func ownCommands(docker bool) []proto.CommandSpec {
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
		{Name: "ext", Group: "tools",
			Description: "extensions: where each comes from, its server, commands and hooks"},
		{Name: "docker", Usage: "[on|off|status]", Group: "tools", Choices: choices("on", "exec in Docker", "off", "exec on the host", "status", ""),
			State: onOff(docker), Description: "where commands run: on, in a Docker container; off, on this machine"},
		{Name: "worktree", Usage: "[status|new|commit]", Group: "git", Choices: choices("status", "", "new", "isolate in a new worktree", "commit", "write the message and ask"),
			Description: "git: alone shows status, new isolates in a worktree, commit writes the message and asks"},
		{Name: "ship", Usage: "[verify|push|pr]", Group: "git", Choices: choices("verify", "fetch, rebase, run checks", "push", "and push", "pr", "and open a PR"),
			Description: "ship the worktree: fetch, rebase, run checks, push (default: push)"},
		{Name: "memories", Group: "memory", Description: "this project's saved notes: when written, refined, confirmed or void"},
		{Name: "remember", Usage: "[text]", Group: "memory", Text: true,
			Description: "save a note for later sessions on this project (needs /raglit on); alone: the model picks what to keep"},
		{Name: "rules", Usage: "[full|summary|index]", Group: "session",
			Choices: choices("full", "the rules files and their imports, whole, in the system prompt",
				"summary", "the imports condensed once by the model (root files stay whole)",
				"index", "the imports in the docs index, surfaced by relevance (needs /raglit on)"),
			Description: "project rules (AGENTS.md, CLAUDE.md, their @imports): alone shows files, cost and mode"},
		{Name: "suggest", Usage: "[on|off|auto]", Group: "session", Choices: choices("on", "always", "off", "never", "auto", "when idle"),
			State: suggestMode, Description: "next-message suggestions: alone asks for some now, on/off/auto set the mode"},
		{Name: "rephrase", Usage: "[on|off|status]", Group: "session", Choices: choices("on", "rewrite prompts first", "off", "as typed", "status", ""),
			State: onOff(rephraseOn), Description: "rephrase prompts for specificity before acting"},
		{Name: "jobs", Usage: "[kill <n>|kill all]", Group: "session", Choices: choices("kill", "kill a job and everything it started: a number, or all"),
			Description: "background jobs: alone lists the running ones, kill <n> ends one and everything it started, kill all ends every one"},
		{Name: "interrupt", Group: "session", Description: "stop the running turn; what it wrote so far stays, and your next message continues from there"},
		{Name: "clear", Group: "session", Description: "clear the conversation and start a fresh session log"},
		{Name: "close", Group: "session", Description: "discard this session for good: remove its worktree and branch, forget its transcript"},
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
		r.server(e.ID, strings.Join(lowerAll(args), " "))
		return
	}
	switch name {
	case "ext":
		if len(args) > 0 {
			r.reply("usage: /ext — lists the extensions")
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
		default:
			r.reply("rephrase: " + onOff(rephraseOn))
		}
	case "suggest":
		r.control(name, arg(0))
		switch arg(0) {
		case "on", "off", "auto":
			r.reply("suggestions: " + arg(0))
		}
	case "docker", "worktree", "ship":
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
		case len(args) == 1 && (arg(0) == dun.RulesFull || arg(0) == dun.RulesSummary || arg(0) == dun.RulesIndex):
			r.control(name, arg(0))
		default:
			r.reply("usage: /rules [full|summary|index]")
		}
	case "memories", "close", "interrupt":
		r.control(name, "")
	case "remember":
		r.control(name, strings.Join(args, " "))
	case "clear":
		r.reset()
	default:
		r.reply("no command /" + name)
	}
}
