// Package enginecmd is what the engine says as data: its slash commands, the
// server states and /mcp info. The engine (--engine) sends them; the TUI and its tests
// read the same lists.
package enginecmd

import (
	"cmp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
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
func CommandList(servers []harness.ServerState, iso string, exts ...mcpserver.Extension) *proto.Commands {
	if len(exts) == 0 {
		exts = mcpserver.BundledExtensions()
	}
	serverState := func(alias string) (string, string) {
		var st harness.ServerState
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
func extCommand(e mcpserver.Extension, state, detail string) proto.CommandSpec {
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
func ExtOwnCommands(e mcpserver.Extension) []string {
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
	if _, own := e.Slash.Commands["setup"]; !own && e.ID == mcpserver.ServerDocs {
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
		{Name: "mcp", Usage: "[info | help | add [preset] | tools [id] | on|off <id> [--global|--project|--session] | unset <id> --project|--session | mode <id> standalone|scripted|unset [--scope] | default standalone|scripted | instance <id> shared|session|agent|unset [--scope] | toggle <id> <tool> on|off|unset [--scope] | remove <id> | refresh <id> | login|logout <id> | restart [id]]", Group: "tools", Text: true,
			Choices: choices("info", "each server's state, mode, tools and tokens; the shell; what scripting a heavy one saves",
				"help", "these actions, one line each",
				"add", "add a server: a form — a preset, or an id and a URL or a local command",
				"on", "turn a server on: this session, or --project / --global", "off", "turn a server off: this session, or --project / --global",
				"unset", "clear a server's --project or --session switch", "restart", "restart servers", "remove", "remove a server from the catalog", "refresh", "re-index a server's docs",
				"mode", "standalone (its own tools, schemas in every request) or scripted (in mcpshell, called through eval), at a scope",
				"default", "the mode of a server no scope sets one for: standalone or scripted, for every project",
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
			State: cmp.Or(iso, harness.IsoNone), Description: "where dun's edits and commands are: in place, a fresh git worktree, or a worktree with commands in Docker; alone shows which",
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
		{Name: "rules", Usage: "[full|summary|index [--scope] | on|off|unset <file|glob> [--scope] | add|remove <path> [--scope]]", Group: "session",
			Choices: choices("full", "the rules files and their imports, whole, in the system prompt",
				"summary", "the imports condensed once by the model (root files stay whole)",
				"index", "the imports in raglit's index, surfaced by relevance (needs /rag on)",
				"on", "a file (or glob) back in every request", "off", "a file (or glob) out of every request, still found through raglit's index (/rag on)",
				"unset", "a file's switch at a scope, back to the one below",
				"add", "make a file a root rules file at a scope (session when none is given); ./path with --project or --global names each project's own file",
				"remove", "take back a file /rules add added at a scope"),
			Description: "project rules (AGENTS.md, CLAUDE.md, their @imports): alone, each file's cost, its switch at each scope and the mode; a mode word sets the project's mode"},
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
		{Name: "journal", Usage: "[view <n> | now]", Group: "session",
			Choices:     choices("view", "one entry's text", "now", "the model writes an entry for everything since the last one: one side call"),
			Description: "what the agent did, in time order: folds, /journal now entries, asks and turns; alone, the list, newest first"},
		{Name: "plan", Usage: "[mode on|off | approve | edit <id> title|details <text> | drop <id> [why] | clear | unarchive | summary]", Group: "session",
			Choices: choices("mode", "on or off: while on, the model's tools that change files or run commands are refused until you approve",
				"approve", "let the model change things in plan mode", "edit", "change a node's title or details", "drop", "drop a node, with a reason",
				"clear", "archive every finished tree: out of the view and the model's plan, kept in the file", "unarchive", "bring the archived trees back",
				"summary", "the model says Done / Doing / Next from the plan and the recent session: one side call, beside the turn"),
			Description: "the task plan the model keeps: alone, the graph as a tree to review, edit, drop and approve; the model is told what you change"},
		{Name: "effort", Usage: "[" + strings.Join(harness.EffortLevels, "|") + "]", Group: "session",
			Choices: effortChoices(),
			State:   EffortLevel, Description: "how much the model thinks before it answers: alone picks a level; saved for every session (off is none)"},
		{Name: "suggest", Usage: "[on|off|auto]", Group: "session", Choices: choices("on", "always", "off", "never", "auto", "when idle"),
			State: SuggestMode, Description: "next-message suggestions: alone asks for some now, on/off/auto set the mode"},
		{Name: "rephrase", Usage: "[on|off|status | prompt [text|default]]", Group: "session", Choices: choices("on", "rewrite prompts first", "off", "as typed", "status", "", "prompt", "show the rephrasing prompt; prompt <text> sets it, prompt default goes back to dun's"),
			State: textfmt.OnOff(RephraseOn), Description: "rephrase prompts for specificity before acting"},
		{Name: "jobs", Usage: "[kill <n>|kill all|clear]", Group: "session", Choices: choices("kill", "kill a job and everything it started: a number, or all", "clear", "drop finished jobs and stopped or dismissed agents from the list; logs and transcripts are kept"),
			Description: "background jobs: alone lists the running ones, kill <n> ends one and everything it started, kill all ends every one, clear drops finished jobs and stopped or dismissed agents from the list (logs and transcripts kept)"},
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
		{Name: "until", Usage: `[turns <N>] [goal <text> [--check "<cmd>"]] [plan] | off`, Group: "session", Text: true,
			Choices: choices("turns", "stop after N turns: turns 10", "goal", `stop when the goal is met: goal "the tests pass", judged by the model after each turn, or --check "make test" (exit 0 is met)`,
				"plan", "stop when no plan node is ready or under way", "off", "one turn per message again"),
			Description: "you're dun when…: dun keeps starting turns itself until a condition fires; alone, the conditions and the run",
			Help: "/until sets stop conditions; while a run is under way dun starts the next turn itself, and stops at the first condition that fires.\n" +
				"turns <N>: N turns have run. goal <text>: the model, asked after each turn, says the goal is met; with --check \"<cmd>\" the command decides (exit 0 is met; run in the session's exec, 10 minutes at most). plan: no plan node is ready or under way.\n" +
				"Give several at once: /until plan turns 15, /until goal \"tests pass\" --check \"make test\" turns 10. goal or plan without turns stops at 20 turns.\n" +
				"A run also stops when you interrupt, a turn fails, or two turns in a row make no progress. A question the model asks you pauses the run; your answer resumes it. Your next message starts the run (or the turn running now is its first); the conditions stay for later messages until /until off.\n" +
				"Messages you type during a run reach the model as usual. Quote a goal that holds the words turns, plan or goal."},
		{Name: "media", Usage: "[images|audio|video|files send|off | cap <size> | scale on|off]", Group: "session",
			Choices: choices("images", "send|off: images a tool returns", "audio", "send|off", "video", "send|off", "files", "send|off",
				"cap", "<size>: the largest media a tool result sends, e.g. cap 20MB", "scale", "on|off: scale an image over the cap down to fit"),
			Description: "media in tool results (an image file_read or an MCP tool returns): alone, what is sent, described or left out; " +
				"the changes wait on agentkit's media policy",
			Help: "/media alone shows what happens to an image, audio, video or file a tool returns this session.\n" +
				"A model recorded without vision gets each image as the vision model's description (vision_model in config.json), or a label saying it was left out.\n" +
				"/media images off, /media cap 20MB and /media scale off change it for the session once agentkit's media policy is in this build; until then they say so."},
		{Name: "interrupt", Group: "session", Description: "stop the running turn; what it wrote so far stays, and your next message continues from there"},
		{Name: "clear", Usage: "[keep|drop] [jobs=keep|stop] [plan=keep|drop]", Group: "session",
			Choices: choices("keep", "keep running jobs and open asks and plan items; the new conversation is told about them",
				"drop", "stop running jobs and drop open asks and plan items"),
			Description: "clear the conversation and start a fresh session log; asks first what becomes of running background jobs and open asks and plan items",
			Help: "/clear starts the conversation again, in a fresh session log (the old one is kept beside it).\n" +
				"Background jobs still running, and open asks and plan items, outlive the conversation, so /clear asks first what becomes of them:\n" +
				"jobs=keep (they run on; the new conversation is told which ones, and hears when each ends) or jobs=stop (killed);\n" +
				"plan=keep (carried over; the new conversation is told about them) or plan=drop (dropped with the conversation).\n" +
				"/clear keep keeps both, /clear drop stops and drops both. With nothing running or open it clears at once. A turn running is stopped once you answer."},
		{Name: "discard", Group: "session", Description: "discard this session for good: remove its worktree and branch, forget its transcript (it was /close; /close now exits, USER 2026-10-27)"},
	}
}

// RephraseOn is the engine-side /rephrase toggle: when true, every user message
// is rephrased for specificity before it starts a turn (see harness.Harness.
// Rephrase). Off by default — the extra LLM round-trip is only worth it when
// asked for. Set by the `rephrase` control command and --rephrase; the TUI keeps
// its own copy for the bare /rephrase status, so the engine's copy only has to
// be right when a turn actually runs.
var RephraseOn bool

// SuggestMode is when a client shows suggestions: on (always), off, auto
// (idle with an empty input). One value per session, shared by every client;
// /suggest on|off|auto sets it.
var SuggestMode = "off"

// EffortLevel is /effort's level, for the command list's state (dun/effort.go).
var EffortLevel = harness.LoadEffort()

// EffortWords is an effort level in words: for default, what this
// engine's server does when its model list says (harness.Harness.EffortWords).
func EffortWords(level string) string {
	if h, _ := EngineHarness.Load().(*harness.Harness); h != nil {
		return h.EffortWords(level)
	}
	return harness.EffortAbout[level]
}

// effortChoices are the /effort picker's rows: every level, and for the
// session's model, which it does not take as asked and why (agentkit's
// EffortSupport, harness.Harness.EffortPicker). Before the harness is up,
// every level as described.
func effortChoices() []proto.CommandChoice {
	h, _ := EngineHarness.Load().(*harness.Harness)
	if h == nil {
		out := make([]proto.CommandChoice, 0, len(harness.EffortLevels))
		for _, l := range harness.EffortLevels {
			out = append(out, proto.CommandChoice{Value: l, Description: EffortWords(l)})
		}
		return out
	}
	var out []proto.CommandChoice
	for _, p := range h.EffortPicker() {
		d := p.About
		if p.Unavailable != "" {
			d = "unavailable on this model: sent as " + p.Unavailable
		}
		out = append(out, proto.CommandChoice{Value: p.Level, Description: d})
	}
	return out
}

// EngineHarness is this engine's harness, once started, for settingsEvent.
var EngineHarness atomic.Value

// ExtIndexVerb reports whether /<slash> <verb> is one of the index commands
// e's roles give it (dun/attach.go): watch with an index CLI, attach and
// detach with a search role. An extension's own command of the same name
// wins (routeSlash asks it first).
func ExtIndexVerb(e mcpserver.Extension, verb string) bool {
	switch verb {
	case "watch":
		return e.Roles.Index != nil && e.Roles.Index.CLI != ""
	case "attach", "detach":
		return e.Roles.Search != nil
	case "workspace": // whether the workspace is read into an index (dun/ragworkspace.go)
		return e.Roles.Search != nil && e.Subscribes(mcpserver.HookWorkspaceOpen)
	}
	return false
}

// ExcludeVerb reports whether /<slash> <verb> is an exclude command: the
// code server's, whose index takes --exclude.
func ExcludeVerb(id, verb string) bool {
	return id == mcpserver.ServerCode && (verb == "exclude" || verb == "include" || verb == "budget" || verb == "index-anyway")
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
	harness.IsoNone, "in place: the checkout itself, commands on this machine",
	harness.IsoWorktree, "a fresh git worktree on a new branch; commands on this machine",
	harness.IsoDocker, "a fresh git worktree, its commands in a Docker container on it",
	"off", "back in place, as none; a worktree is kept on disk, never deleted")

// PresetIDs lists the presets for a hint.
func PresetIDs() string {
	ids := make([]string, len(mcpserver.Presets()))
	for i, p := range mcpserver.Presets() {
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
