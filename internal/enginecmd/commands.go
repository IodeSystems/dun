// Package enginecmd is what the engine says as data: its slash commands, the
// server states and /mcp info. The -p engine sends them; the TUI and its tests
// read the same lists.
package enginecmd

import (
	"cmp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

func choices(pairs ...string) []proto.CommandChoice {
	out := make([]proto.CommandChoice, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, proto.CommandChoice{Value: pairs[i], Description: pairs[i+1]})
	}
	return out
}

// CommandList builds the list from the state it shows: the extensions'
// slash commands (the bundled ones when exts is empty), after dun's own.
func CommandList(servers []dun.ServerState, iso string, exts ...dun.Extension) *proto.Commands {
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
		return textfmt.OnOff(st.Running), detail
	}
	var items []proto.CommandSpec
	for _, e := range exts {
		if e.Slash == nil {
			continue
		}
		state, detail := serverState(e.ID)
		items = append(items, extCommand(e, state, detail))
	}
	return &proto.Commands{Items: append(OwnCommands(iso), items...)}
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
	own := ExtOwnCommands(e)
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
		Description: dunconfig.FirstNonEmpty(e.Slash.Description, e.Description)}
}

// ExtOwnCommands are e's commands beside the server switch, as name, help
// pairs: the manifest's (by name), then the index commands its roles give it.
func ExtOwnCommands(e dun.Extension) []string {
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
	if _, own := e.Slash.Commands["watch"]; !own && ExtIndexVerb(e, "watch") {
		pairs = append(pairs, "watch", "watch a directory into an index of this project: watch <dir> [--index NAME]")
	}
	if _, own := e.Slash.Commands["exclude"]; !own && ExcludeVerb(e.ID, "exclude") {
		pairs = append(pairs, excludeHelp...)
	}
	if _, own := e.Slash.Commands["setup"]; !own && e.ID == dun.ServerDocs {
		pairs = append(pairs, ragSetupHelp...)
	}
	if _, own := e.Slash.Commands["attach"]; !own && ExtIndexVerb(e, "attach") {
		pairs = append(pairs, "attach", "search another index too: attach <index>|<project>:<index> [--project|--global]; bare lists",
			"detach", "stop searching an attached index: detach <index> [--project|--global]")
	}
	if _, own := e.Slash.Commands["workspace"]; !own && ExtIndexVerb(e, "workspace") {
		pairs = append(pairs, "workspace", "whether this workspace is read into an index: workspace [on|off] [--session|--project|--global]; off keeps search on other indexes")
	}
	return pairs
}

// OwnCommands are dun's own engine commands, which no extension's slash
// command may shadow.
func OwnCommands(iso string) []proto.CommandSpec {
	// In the order a client lists them within a group: /mcp, which a new
	// user adds a server with, before /ext, which explains extensions.
	return []proto.CommandSpec{
		{Name: "mcp", Usage: "[info | help | add [preset] | tools [id] | on|off <id> [--global|--project|--session] | unset <id> --project|--session | mode <id> standalone|scripted|unset [--scope] | instance <id> shared|session|agent|unset [--scope] | toggle <id> <tool> on|off|unset [--scope] | remove <id> | refresh <id> | login|logout <id> | restart [id]]", Group: "tools", Text: true,
			Choices: choices("info", "each server's state, mode, tools and tokens; the shell; what scripting a heavy one saves",
				"help", "these actions, one line each",
				"add", "add a server: a form — a preset, or an id and a URL or a local command",
				"on", "turn a server on: this session, or --project / --global", "off", "turn a server off: this session, or --project / --global",
				"unset", "clear a server's --project or --session switch", "restart", "restart servers", "remove", "remove a server from the catalog", "refresh", "re-index a server's docs",
				"mode", "standalone (its own tools, schemas in every request) or scripted (in mcpshell, called through eval), at a scope",
				"instance", "shared (one connection in dund for every session), session (this session's own) or agent (one more per sub-agent), at a scope",
				"toggle", "turn one tool on or off, at a scope",
				"tools", "what each server's tools cost; with an id, list them and turn some off",
				"login", "log in to an oauth server in the browser", "logout", "delete (and revoke) an oauth server's token"),
			Description: "tool servers (MCP): see them and turn them on or off; /mcp add adds one",
			Help: "/mcp alone opens a table of every server: on or off, and where that is set (this session, this project, every project).\n" +
				"/mcp add opens a form: a name, and the command or web address you were given. A preset fills the server in from its name: " + PresetIDs() + ".\n" +
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
		{Name: "memories", Usage: "[add memory|strategy|constraint [global] <name> [detail] | rm|important|unimportant|accept|reject <kind:name> | candidates]", Group: "memory",
			Choices: choices("add", "save a memory, strategy or constraint", "rm", "void one (kept on disk)",
				"important", "list one by path in every session", "unimportant", "back to found by search only",
				"candidates", "strategies the learning phase proposed", "accept", "use a candidate", "reject", "void a candidate"),
			Description: "this project's notes, strategies and constraints: alone, each one and what has happened to it"},
		{Name: "remember", Usage: "[text]", Group: "memory", Text: true,
			Description: "save a note for later sessions on this project (needs /rag on); alone: the model picks what to keep"},
		{Name: "rules", Usage: "[full|summary|index [--scope] | on|off|unset <file|glob> [--scope]]", Group: "session",
			Choices: choices("full", "the rules files and their imports, whole, in the system prompt",
				"summary", "the imports condensed once by the model (root files stay whole)",
				"index", "the imports in the docs index, surfaced by relevance (needs /rag on)",
				"on", "a file (or glob) back in every request", "off", "a file (or glob) out of every request, still found through the docs index",
				"unset", "a file's switch at a scope, back to the one below"),
			Description: "project rules (AGENTS.md, CLAUDE.md, their @imports): alone, each file's cost, its switch at each scope and the mode; a mode word sets the project's mode"},
		{Name: "spend", Usage: "[reconcile <usd> | budget <usd>|off]", Group: "session",
			Description: "estimated spend this month and the budget left; reconcile takes the real figure off the Console"},
		{Name: "loops", Usage: "[off|count|cut | tolerance N|default | tools N|count|default]", Group: "session",
			Choices: choices("status", "the settings now, and what has been found", "off", "only the guard on the same bytes back to back",
				"count", "find loops and count them; cut nothing the guard would not",
				"cut", "cut a reply that continues a loop, which retries it",
				"tolerance", "the loop count that cuts: tolerance N or default",
				"tools", "the same for tool calls: tools N, count or default"),
			Description: "loop detection in the model's replies: alone says what it has found; saved for every session"},
		{Name: "retry", Usage: "[default|patient|stubborn | attempts N | budget <dur>|unbounded|default | turn <dur>|off|default]", Group: "session",
			Choices: choices("status", "the settings now", "default", RetryPresets[0].about, "patient", RetryPresets[1].about,
				"stubborn", RetryPresets[2].about, "attempts", "how many times a 5xx is retried: attempts N",
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
		{Name: "plan", Usage: "[mode on|off | approve | edit <id> title|goal <text> | drop <id> [why]]", Group: "session",
			Choices: choices("mode", "on or off: while on, the model's tools that change files or run commands are refused until you approve",
				"approve", "let the model change things in plan mode", "edit", "change a node's title or goal", "drop", "drop a node, with a reason"),
			Description: "the task plan the model keeps: alone, the graph as a tree to review, edit, drop and approve; the model is told what you change"},
		{Name: "effort", Usage: "[default|off|low|medium|high]", Group: "session",
			Choices: choices(dun.EffortDefault, EffortWords(dun.EffortDefault), dun.EffortOff, dun.EffortAbout[dun.EffortOff],
				dun.EffortLow, dun.EffortAbout[dun.EffortLow], dun.EffortMedium, dun.EffortAbout[dun.EffortMedium],
				dun.EffortHigh, dun.EffortAbout[dun.EffortHigh]),
			State: EffortLevel, Description: "how much the model thinks before it answers: alone picks a level; saved for every session"},
		{Name: "suggest", Usage: "[on|off|auto]", Group: "session", Choices: choices("on", "always", "off", "never", "auto", "when idle"),
			State: SuggestMode, Description: "next-message suggestions: alone asks for some now, on/off/auto set the mode"},
		{Name: "rephrase", Usage: "[on|off|status | prompt [text|default]]", Group: "session", Choices: choices("on", "rewrite prompts first", "off", "as typed", "status", "", "prompt", "show the rephrasing prompt; prompt <text> sets it, prompt default goes back to dun's"),
			State: textfmt.OnOff(RephraseOn), Description: "rephrase prompts for specificity before acting"},
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

// RephraseOn is the engine-side /rephrase toggle: when true, every user message
// is rephrased for specificity before it starts a turn (see dun.Harness.
// Rephrase). Off by default — the extra LLM round-trip is only worth it when
// asked for. Set by the `rephrase` control command and --rephrase; the TUI keeps
// its own copy for the bare /rephrase status, so the engine's copy only has to
// be right when a turn actually runs.
var RephraseOn bool

// SuggestMode is when a client shows suggestions: on (always), off, auto
// (idle with an empty input). One value per session, shared by every client;
// /suggest on|off|auto sets it.
var SuggestMode = "auto"

// EffortLevel is /effort's level, for the command list's state (dun/effort.go).
var EffortLevel = dun.LoadEffort()

// EffortWords is an effort level in words: for default, what this
// engine's server does when its model list says (dun.Harness.EffortWords).
func EffortWords(level string) string {
	if h, _ := EngineHarness.Load().(*dun.Harness); h != nil {
		return h.EffortWords(level)
	}
	return dun.EffortAbout[level]
}

// EngineHarness is this engine's harness, once started, for settingsEvent.
var EngineHarness atomic.Value

// ExtIndexVerb reports whether /<slash> <verb> is one of the index commands
// e's roles give it (dun/attach.go): watch with an index CLI, attach and
// detach with a search role. An extension's own command of the same name
// wins (routeSlash asks it first).
func ExtIndexVerb(e dun.Extension, verb string) bool {
	switch verb {
	case "watch":
		return e.Roles.Index != nil && e.Roles.Index.CLI != ""
	case "attach", "detach":
		return e.Roles.Search != nil
	case "workspace": // whether the workspace is read into an index (dun/ragworkspace.go)
		return e.Roles.Search != nil && e.Subscribes(dun.HookWorkspaceOpen)
	}
	return false
}

// ExcludeVerb reports whether /<slash> <verb> is an exclude command: the
// code server's, whose index takes --exclude.
func ExcludeVerb(id, verb string) bool {
	return id == dun.ServerCode && (verb == "exclude" || verb == "include" || verb == "budget" || verb == "index-anyway")
}

// excludeHelp is exclude's and include's lines in the palette and in bare
// /lsp.
var excludeHelp = []string{
	"exclude", "DIR — leave a directory (relative to the workspace) out of the code index, for this project; the server restarts. The repository is not changed",
	"include", "DIR — put an excluded directory back in the code index (the undo of exclude, and of what dun excluded when the index did not fit its budget)",
	"index-anyway", "on|off — index a root that is not a project (the home directory, a directory of clones) anyway, for this project; the server restarts; bare shows it",
	"budget", "Nms|Nops|0|default — the code index's startup budget, for this project (0: none; default: the server's, 30000ms); bare shows it",
}

// isolateChoices are /isolate's values, for the palette and /help.
var isolateChoices = choices(
	dun.IsoNone, "in place: the checkout itself, commands on this machine",
	dun.IsoWorktree, "a fresh git worktree on a new branch; commands on this machine",
	dun.IsoDocker, "a fresh git worktree, its commands in a Docker container on it",
	"off", "back in place, as none; a worktree is kept on disk, never deleted")

// PresetIDs lists the presets for a hint.
func PresetIDs() string {
	ids := make([]string, len(dun.Presets()))
	for i, p := range dun.Presets() {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}

// ragSetupHelp is setup's line in the palette and in bare /rag.
var ragSetupHelp = []string{"setup", "pick a model for each role (embeddings, summaries, vision, segment, audio) from your providers; none turns one off"}

type retryPreset struct {
	Name, about  string
	Attempts     int
	Budget, Turn time.Duration
}

var RetryPresets = []retryPreset{
	{"default", "5 tries · a request up to 5m · a dead stream re-run for 5m", 0, 0, 0},
	{"patient", "10 tries · a request up to 15m · a dead stream re-run for 30m", 10, 15 * time.Minute, 30 * time.Minute},
	{"stubborn", "20 tries · a request up to 1h · a dead stream re-run for 2h", 20, time.Hour, 2 * time.Hour},
}
