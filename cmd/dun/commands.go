package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// The slash commands the ENGINE carries out, as data (proto.Commands) and as
// one router (routeSlash). A client that is not the TUI — the web app, the
// phone — gets the list from the commands event and sends a typed line as
// control "slash"; the TUI keeps its own table for the commands only it can
// run (mouse, trace, forms, pickers), and a test pins its engine-routed
// entries to this list.

// commandsEvent is the list, with the current state of each toggle.
func commandsEvent(h *dun.Harness) *proto.Commands {
	exts, _ := engineExtensions(h.Extensions())
	return enginecmd.CommandList(h.Servers(), isolationMode(h), exts...)
}

// providersMu orders provider changes and model listings in the engine, so
// the last models event a client gets reflects the last change: a listing
// started just before an edit cannot land after it with the old key.
var providersMu sync.Mutex

// modelsEvent lists every saved provider's models, fetched in parallel.
// Keys stay here; a client gets names, URLs and ids.
func modelsEvent() *proto.Models {
	cfg := dunconfig.LoadConfig()
	ps, def := cfg.ProviderList(), cfg.ActiveName()
	out := make([]proto.ModelProvider, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, why := dunconfig.ListModels(p.URL, p.Key)
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
	if name == "poly-lsp" { // its name until /lsp (2026-10-29)
		name = "lsp"
	}
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
		if enginecmd.ExtIndexVerb(e, arg(0)) {
			r.control("ext", strings.TrimSpace(arg(0)+" "+e.ID+" "+strings.Join(args[1:], " ")))
			return
		}
		// exclude|include <dir>: the directory keeps its case.
		if enginecmd.ExcludeVerb(e.ID, arg(0)) {
			r.server(e.ID, strings.TrimSpace(arg(0)+" "+strings.Join(args[1:], " ")))
			return
		}
		// /lsp add|remove: a command line for poly-lsp-mcp, its case kept.
		if e.ID == dun.ServerCode && (arg(0) == "add" || arg(0) == "remove") {
			r.server(e.ID, arg(0)+" "+strings.Join(args[1:], " "))
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
			r.reply(enginecmd.McpHelp())
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
			if a := arg(0); a != "" && !enginecmd.ServerActions[a] {
				r.reply(enginecmd.UnknownMCPAction(a))
				return
			}
			target := enginecmd.AllServers
			if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
				target = arg(1)
			}
			action := arg(0)
			if len(args) > 2 {
				action += " " + strings.Join(lowerAll(args[2:]), " ")
			}
			if target == enginecmd.AllServers && len(args) > 1 {
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
			r.reply("rephrase: " + textfmt.OnOff(enginecmd.RephraseOn))
		}
	case "worklog":
		// The title and the why as typed.
		switch arg(0) {
		case "", "start", "end", "view", "write":
			r.control(name, strings.TrimSpace(arg(0)+" "+strings.Join(args[min(1, len(args)):], " ")))
		default:
			r.reply("usage: /worklog [start <title> | end [done|dropped|blocked] <why> | view [n] | write [n]]")
		}
	case "plan":
		// Text as typed: a title or a reason keeps its case.
		switch arg(0) {
		case "":
			r.control(name, "view")
		case "mode", "approve", "edit", "drop":
			r.control(name, strings.TrimSpace(arg(0)+" "+strings.Join(args[min(1, len(args)):], " ")))
		default:
			r.reply("usage: /plan [mode on|off | approve | edit <id> title|goal <text> | drop <id> [why]]")
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
	case "retry", "loops", "spend":
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
		_, rest := textfmt.CutFields(strings.TrimPrefix(strings.TrimSpace(line), "/"), 1)
		r.control(name, rest)
	case "clear":
		r.reset()
	default:
		r.reply("no command /" + name)
	}
}
