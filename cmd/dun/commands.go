package main

import (
	"strings"
	"sync"

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
	return commandList(h.Servers(), h.IsDocker())
}

// commandList builds the list from the state it shows.
func commandList(servers []dun.ServerState, docker bool) *proto.Commands {
	serverState := func(alias string) (string, string) {
		var st dun.ServerState
		ok := false
		for _, s := range servers {
			if s.ID == serverAliases[alias] {
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
	srvChoices := choices("on", "start it now", "off", "stop it", "restart", "restart it",
		"auto", "start it every session", "manual", "stop starting it every session")
	ragState, ragDetail := serverState("rag")
	lspState, lspDetail := serverState("lsp")
	return &proto.Commands{Items: []proto.CommandSpec{
		{Name: "rag", Usage: "[on|off|restart|auto|manual]", Group: "tools", Choices: srvChoices, State: ragState, Detail: ragDetail,
			Description: "docs index (raglit): bare shows status, auto starts it every session"},
		{Name: "lsp", Usage: "[on|off|restart|auto|manual]", Group: "tools", Choices: srvChoices, State: lspState, Detail: lspDetail,
			Description: "code intelligence (poly-lsp-mcp): bare shows status, auto starts it every session"},
		{Name: "mcp", Usage: "[remove <id> | refresh <id> | on|off|restart [id]]", Group: "tools", Text: true,
			Choices:     choices("on", "start servers", "off", "stop servers", "restart", "restart servers", "remove", "remove a server", "refresh", "re-index a server's docs"),
			Description: "MCP servers: bare lists them; refresh re-indexes a server's docs (adding one is in the TUI: /mcp add)"},
		{Name: "docker", Usage: "[on|off|status]", Group: "tools", Choices: choices("on", "exec in Docker", "off", "exec on the host", "status", ""),
			State: onOff(docker), Description: "exec backend: on (Docker), off (host), bare shows status"},
		{Name: "worktree", Usage: "[status|new|commit]", Group: "git", Choices: choices("status", "", "new", "isolate in a new worktree", "commit", "write the message and ask"),
			Description: "git: bare shows status, new isolates in a worktree, commit writes the message and asks"},
		{Name: "ship", Usage: "[verify|push|pr]", Group: "git", Choices: choices("verify", "fetch, rebase, run checks", "push", "and push", "pr", "and open a PR"),
			Description: "ship the worktree: fetch, rebase, run checks, push (default: push)"},
		{Name: "memories", Group: "memory", Description: "this project's saved notes: when written, refined, confirmed or void"},
		{Name: "remember", Usage: "[text]", Group: "memory", Text: true,
			Description: "save a note for later sessions on this project (needs /rag on); bare: the model picks what to keep"},
		{Name: "suggest", Usage: "[on|off|auto]", Group: "session", Choices: choices("on", "always", "off", "never", "auto", "when idle"),
			State: suggestMode, Description: "next-message suggestions: bare asks for some now, on/off/auto set the mode"},
		{Name: "rephrase", Usage: "[on|off|status]", Group: "session", Choices: choices("on", "rewrite prompts first", "off", "as typed", "status", ""),
			State: onOff(rephraseOn), Description: "rephrase prompts for specificity before acting"},
		{Name: "clear", Group: "session", Description: "clear the conversation and start a fresh session log"},
		{Name: "close", Group: "session", Description: "discard this session for good: remove its worktree and branch, forget its transcript"},
	}}
}

// modelsEvent lists every saved provider's models, fetched in parallel.
// Keys stay here; a client gets names, URLs and ids.
func modelsEvent() *proto.Models {
	cfg := loadConfig()
	ps, def := cfg.providerList(), cfg.active().Name
	out := make([]proto.ModelProvider, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, why := listModels(p.URL, p.Key)
			out[i] = proto.ModelProvider{Name: p.Name, URL: p.URL, IDs: ids, Error: why,
				Default: p.Name == def, Key: p.Key != ""}
		}()
	}
	wg.Wait()
	return &proto.Models{Providers: out}
}

// slashRoutes are what routeSlash needs from the engine.
type slashRoutes struct {
	server  func(alias, action string)
	control func(id, action string)
	reset   func()
	reply   func(msg string) // an answer that is not a command's own event
}

// routeSlash carries out one slash line ("rag on", "/remember the tests are
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
	switch name {
	case "rag", "lsp":
		r.server(name, arg(0))
	case "mcp":
		switch arg(0) {
		case "add":
			r.reply("adding an MCP server takes the form in the TUI (/mcp add) for now")
		case "remove", "rm", "refresh":
			if len(args) != 2 {
				r.reply("usage: /mcp " + arg(0) + " <id>")
				return
			}
			r.control("mcp", arg(0)+" "+args[1])
		default:
			target := allServers
			if len(args) > 1 {
				target = arg(1)
			}
			r.server(target, arg(0))
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
	case "memories", "close":
		r.control(name, "")
	case "remember":
		r.control(name, strings.Join(args, " "))
	case "clear":
		r.reset()
	default:
		r.reply("no command /" + name)
	}
}
