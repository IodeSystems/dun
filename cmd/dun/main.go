// Command dun (Slice 1): compose poly-lsp-mcp + mcpshell + raglit into an
// agentkit loop and work a task in a workspace.
//
//	dun [--workspace DIR] [--model M] "your task"     human-readable stream
//	dun -p [--workspace DIR] ["first task"]           programmatic: line-delimited
//	                                                  JSON events in/out
//
// The Bubble Tea TUI (Slice 2) is a CONSUMER of the -p event stream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/openurl"
	"github.com/iodesystems/dun/internal/spendmeter"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/proto"
)

// version is stamped at build time via -ldflags "-X main.version=…" (see the
// Makefile). "dev" for a plain `go build`. Shown by `dun -version` and in the
// TUI header so a stale on-PATH binary is visible at a glance.
var version = "dev"

func init() {
	buildinfo.Version, buildinfo.Channel, buildinfo.SrcDir = version, channel, srcDir
}

// defaultDockerImage is the container used when --docker is passed without an
// explicit image. Ubuntu 24.04 is the least surprising environment for an LLM:
// apt, bash, git, and a familiar filesystem.
const defaultDockerImage = "ubuntu:24.04"

// resolveDockerImage turns the raw --docker flag value into a real image name.
// "true" → defaultDockerImage, anything else → as-is, "" → "" (off).
func resolveDockerImage(raw string) string {
	if raw == "true" {
		return defaultDockerImage
	}
	return raw
}

// checkDockerFlag refuses a --docker image other than the bundled one: the
// whole of dun runs in that container, and only the bundled image holds it.
func checkDockerFlag(image string) error {
	if image == "" || image == defaultDockerImage {
		return nil
	}
	return fmt.Errorf("%q is not the bundled image. --docker runs dun itself in the container, "+
		"which only the bundled image (--docker, or --docker=true) can hold. "+
		"To run commands in your own image, start dun without --docker and use: /isolate docker image %s", image, image)
}

// usage prints what dun is and how to start, then the flags by group, as
// --long-flags.
//
// Go's flag package accepts -tui and --tui identically but PRINTS the first,
// and `-tui` reads like the three short flags -t -u -i. Only genuinely
// single-letter flags keep one dash.
func usage() { writeUsage(os.Stderr, flag.CommandLine) }

// flagGroups orders --help; a flag in none of them is listed under the last.
var flagGroups = []struct {
	title string
	names []string
}{
	{"start and sessions", []string{"setup", "continue", "resume", "new", "sessions", "tui", "web-tui", "addr", "p", "version"}},
	{"model and provider", []string{"url", "model", "key", "child-model", "timeout", "timeout-grace", "no-suggest", "rephrase", "bare"}},
	{"workspace and isolation", []string{"workspace", "worktree", "docker", "docker-network", "rag", "lsp", "no-rag", "no-lsp", "no-tools", "media"}},
	{"experimental", []string{"exp-iterm-tabcontrol"}},
	{"advanced", []string{"persist", "disable-exit", "replay", "replay-speed", "input-delay", "max-gap"}},
}

// removedFlags are flags that no longer do anything. They stay accepted
// because an older TUI or dund passes them to a newer engine and a script may
// too; --help does not list them.
var removedFlags = map[string]bool{"ship": true, "no-ship": true, "pr": true}

// warnShipRemoved prints one line to stderr when a removed ship flag was given
// on a direct command line.
func warnShipRemoved() {
	given := false
	flag.Visit(func(f *flag.Flag) {
		if removedFlags[f.Name] {
			given = true
		}
	})
	if given {
		fmt.Fprintln(os.Stderr, "dun: ship was removed; --ship, --pr and --no-ship do nothing. The repo's own rules for shipping apply.")
	}
}
func writeUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `dun — a coding assistant in your terminal: it works a task in your project.

start:
  cd your-project && dun     open dun there, then /login (enter on the empty prompt
                             opens it) to connect an AI service, and ask
  dun "fix the flaky test"   run one task headless, print the diff, exit
  dun --continue             reopen the most recent session for this workspace
  dun doctor [--fix]         check providers, tools, dund and the service; --fix runs what it can
  dun tools [--upgrade]      the tools against the versions this dun pins; --upgrade installs them (dun tools --help)
  dun tmux                   the tmux.conf lines that show dun's state in the window list
  dun mcp                    dun's sessions as an MCP server on stdio, for another agent (dun mcp --help)
  dun watch <session>        a line each time a session ends a turn or asks (dun watch --help)
  dun msg                    message dun sessions and listen for messages from a shell (dun msg --help)
  dun -p                     engine mode: line-delimited JSON events on stdin/stdout
  dun dund …                 the session service (dun dund --help)

  DUN_URL, DUN_MODEL, DUN_LLM_KEY in the environment set the provider without
  /login (--url, --model, --key for one run).
`)
	line := func(f *flag.Flag) {
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		name, help := flag.UnquoteUsage(f)
		head := "  " + dash + f.Name
		if name != "" {
			head += " " + name
		}
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0s" {
			help += " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(w, "%-24s %s\n", head, help)
	}
	grouped := map[string]bool{}
	for n := range removedFlags {
		grouped[n] = true // accepted, hidden
	}
	for _, g := range flagGroups {
		for _, n := range g.names {
			grouped[n] = true
		}
	}
	for i, g := range flagGroups {
		var fl []*flag.Flag
		for _, n := range g.names {
			if f := fs.Lookup(n); f != nil {
				fl = append(fl, f)
			}
		}
		if i == len(flagGroups)-1 {
			fs.VisitAll(func(f *flag.Flag) {
				if !grouped[f.Name] {
					fl = append(fl, f)
				}
			})
		}
		if len(fl) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, f := range fl {
			line(f)
		}
	}
}
func main() {
	// `dun dund …` is the session service (internal/dundcli), in this binary
	// so the service and the engines it runs are always the same build.
	if len(os.Args) > 1 && os.Args[1] == "dund" {
		dundcli.Version = buildinfo.Version
		os.Exit(dundcli.Main(os.Args[2:]))
	}
	// The registry's manifests, presets and pins, as cached (registry.go):
	// before anything reads a manifest.
	useRegistry()
	// `dun session <id> info [--context]`: what dun is in that session, as
	// JSON (sessioninfo.go).
	if len(os.Args) > 1 && os.Args[1] == "session" {
		os.Exit(sessionCmd(os.Args[2:]))
	}
	// `dun oauth token --spec …`: an OAuth MCP server's bearer, for
	// mcpshell's auth_cmd (oauthcmd.go).
	if len(os.Args) > 1 && os.Args[1] == "oauth" {
		os.Exit(oauthCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	// `dun --usage [--json] [--no-net]`: what dun has used, from the stats
	// ledger, and what the provider says is left (usage.go). No session.
	if len(os.Args) > 1 && (os.Args[1] == "--usage" || os.Args[1] == "-usage") {
		os.Exit(usageCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	// `dun doctor`: what dun needs, checked (doctor.go); --fix applies the
	// fixes it can run itself.
	if len(os.Args) > 1 && os.Args[1] == "spend" {
		os.Exit(spendCmd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		c := doctorCmd()
		c.SetArgs(os.Args[2:])
		if err := c.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, "dun doctor:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	// `dun tools`: the tools against the pinned versions; --upgrade installs
	// them (toolscmd.go, /upgrade).
	if len(os.Args) > 1 && os.Args[1] == "tools" {
		os.Exit(toolsCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	// `dun msg`: message sessions and listen for messages from a shell (msgcmd.go).
	if len(os.Args) > 1 && os.Args[1] == "msg" {
		os.Exit(msgCmd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	// `dun mcp`: dun's sessions as an MCP server on stdio (mcpserve.go).
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcpCmd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	// `dun watch`: a line each time a session ends a turn or asks (watchcmd.go).
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		os.Exit(watchCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	// `dun tmux`: the tmux.conf lines that show dun's window mark (tmux.go).
	if len(os.Args) > 1 && os.Args[1] == "tmux" {
		os.Exit(tmuxCmd(os.Stdout, os.Getenv("TMUX") != "", os.Getenv("LC_TERMINAL") == "iTerm2", execTmuxQuery))
	}
	flag.Usage = usage
	ver := flag.Bool("version", false, "print version and exit")
	setup := flag.Bool("setup", false, "run the interactive setup wizard (LLM url/model/key) and exit")
	// Flag defaults come from env then the saved config — so a CLI flag still
	// overrides for a one-off run. There is no built-in provider. See config.go.
	fc := dunconfig.LoadConfig()
	act, _ := fc.Active()
	url := flag.String("url", dunconfig.FirstNonEmpty(os.Getenv("DUN_URL"), act.URL), "LLM base URL (default: the logged-in provider; /login)")
	model := flag.String("model", dunconfig.FirstNonEmpty(os.Getenv("DUN_MODEL"), fc.Model), "chat model (must support tool calls; default: the saved one, /model)")
	// Key default stays empty so `dun -h` never prints the secret; the real value
	// is resolved after parse (flag > env > config).
	key := flag.String("key", "", "API key (set via $DUN_LLM_KEY or 'dun --setup')")
	ws := flag.String("workspace", ".", "workspace directory (worked in place; --worktree isolates a git repo)")
	docker := flag.String("docker", "", "run the entire process in the bundled Docker image (--docker=true); for commands in your own image, start a session and use /isolate docker image <image|Dockerfile>")
	dockerNetwork := flag.Bool("docker-network", true, "give the Docker container network access (default: on, so Go toolchain downloads work)")
	worktree := flag.Bool("worktree", false, "create a git worktree for isolation (default: work in place); in a session, /isolate worktree")
	childModel := flag.String("child-model", "", "model for spawned sub-agents (default: same as --model)")
	childProvider := flag.String("child-provider", "", "logged-in provider --child-model is on (default: this session's)")
	childEffort := flag.String("child-effort", "", "effort for spawned sub-agents: default, off, low, medium, high (default: the saved /effort level)")
	// ship was removed 2026-10-27: repos state their own shipping rules. The
	// flags stay accepted and do nothing, because an older TUI or dund passes
	// them to this engine and a script may too.
	flag.Bool("pr", false, "removed; does nothing")
	flag.Bool("ship", true, "removed; does nothing")
	flag.Bool("no-ship", false, "removed; does nothing")
	cont := flag.Bool("continue", false, "resume the most recent session for this workspace")
	resume := flag.String("resume", "", "resume a specific session id (see --sessions)")
	// Set by dund, like --resume: the session persists, so its sub-agents
	// do too.
	persistFlag := flag.Bool("persist", false, "set by dund: the session persists, so resuming it restarts its running and idle sub-agents")
	noDir := flag.Bool("no-dir", false, "a session attached to no directory: talk with dun, its skills and MCP tools; no project files, rules, code or document index (sessions live in $DUN_HOME/nodir)")
	fresh := flag.Bool("new", false, "start a new session, even when this workspace has one no client is attached to (which plain dun resumes)")
	listSessions := flag.Bool("sessions", false, "list saved sessions for this workspace and exit")
	prog := flag.Bool("p", false, "programmatic mode: emit + read line-delimited JSON events")
	asideEscalateMS := flag.Int("aside-escalate-ms", 0, "how long a heartbeat about a silent background job or child waits for a turn to carry it before it wakes the agent (0: DUN_ASIDE_ESCALATE_MS, else 5000)")
	turnLimit := flag.Int("tool-call-turn-limit", 0, "the most tool-call rounds one turn may make (0: DUN_MAX_TURNS, else 400); the model is told it, and warned at 80%")
	settleDelay := flag.Duration("settle-delay", 0, "interactive: how long after a turn that used tools, with no reply and no sign of the person at the TUI, the agent is woken to report what is done, left, or needs them (0 is off; unset: DUN_SETTLE_DELAY, else 2m)")
	planNudgeRounds := flag.Int("plan-nudge-rounds", 0, "after this many rounds in a turn with no open plan node, the main agent is made to write the plan, once per turn (0 is off; unset: DUN_PLAN_NUDGE_ROUNDS, else 25)")
	planStallRounds := flag.Int("plan-stall-rounds", 0, "after this many rounds in a turn with open plan nodes and no node moved, the main agent is made to update the plan, at most 3 times per turn (0 is off; unset: DUN_PLAN_STALL_ROUNDS, else 30)")
	planNudgeFails := flag.Int("plan-nudge-fails", 0, "after this many failed tool results in a row with no open plan node, the main agent is made to write the plan, once per turn (0 is off; unset: DUN_PLAN_NUDGE_FAILS, else 4)")
	nonInteractive := flag.Bool("non-interactive", false, "with -p: a run no one answers or reads along (a script, a benchmark): no ask_user, and the model is told it is on its own")
	tuiFlag := flag.Bool("tui", false, "launch the interactive Bubble Tea UI")
	// --web-tui: this terminal TUI in a browser tab, byte for byte through a
	// pty (serveterm.go). Not dund's web app (dund --web-experimental), which is its own
	// client of the same sessions; this is the passthrough (USER 2026-10-28:
	// "the web terminal should be --web-tui").
	serve := flag.Bool("web-tui", false, "the terminal TUI in a browser tab (xterm.js over a pty) at --addr")
	addr := flag.String("addr", "127.0.0.1:8734", "web-tui: HTTP listen address")
	disableExit := flag.Bool("disable-exit", false, "TUI: ctrl+c / esc don't quit (exit via /exit)")
	noSuggest := flag.Bool("no-suggest", false, "disable next-message suggestions (on by default)")
	// A TUI flag: it changes only this client's terminal, so procArgs does
	// not pass it to the engine or dund (iterm.go).
	itermTabs := flag.Bool("exp-iterm-tabcontrol", false, "experimental, iTerm2 only: title and colour the tab by what dun waits on (working, question, done)")
	rephrase := flag.Bool("rephrase", false, "rephrase each user prompt for specificity before acting on it (toggle at runtime: /rephrase on)")

	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout")
	timeoutGrace := flag.Duration("timeout-grace", 0, "after --timeout, time the model gets to wrap up before the cut (0 = 20% of --timeout, 2m-10m; negative = none)")
	replay := flag.String("replay", "", "replay a recorded session trace into the TUI (record one with DUN_TRACE=path)")
	replaySpeed := flag.Float64("replay-speed", 1, "replay: pacing multiplier over the recorded timing (2 = twice as fast)")
	inputDelay := flag.Duration("input-delay", -1, "replay: fixed gap between events, overriding the recording (0 = no delay); negative = use the recorded timing")
	maxGap := flag.Duration("max-gap", 2*time.Second, "replay: compress idle gaps longer than this (0 = replay the recording verbatim)")
	// Tri-state: unset means "whatever /rag auto or /lsp auto saved", which a
	// plain bool cannot express (its zero value would silently mean "off").
	var ragFlag, lspFlag tristate
	flag.Var(&ragFlag, "rag", "start the docs server (raglit) this run: --rag or --rag=false (default: the saved setting)")
	flag.Var(&lspFlag, "lsp", "start the code server (poly-lsp-mcp) this run: --lsp or --lsp=false (default: the saved setting)")
	// --no-rag and --no-lsp are --rag=false and --lsp=false (noAutostart).
	noRag := flag.Bool("no-rag", false, "do not start the docs server (raglit) this run; same as --rag=false")
	noLsp := flag.Bool("no-lsp", false, "do not start the code server (poly-lsp-mcp) this run; same as --lsp=false")
	noTools := flag.Bool("no-tools", false, "a plain chat: the model gets no tools at all, and no tool server starts")
	var media mediaFlag
	flag.Var(&media, "media", "attach an image (png, jpeg, gif, webp) to the task's first message; repeat for more: dun --media shot.png \"what is wrong here\"")
	flag.Parse()
	if err := noAutostart(&ragFlag, *noRag, "rag"); err != nil {
		fatal(err)
	}
	if err := noAutostart(&lspFlag, *noLsp, "lsp"); err != nil {
		fatal(err)
	}
	if len(media) > 0 {
		if *docker != "" {
			fatal(fmt.Errorf("--media does not work with --docker: the file is outside the container"))
		}
		if err := media.resolve(); err != nil {
			fatal(err)
		}
	}

	// Full Docker mode: when --docker is set and we are NOT already inside a
	// container, re-exec the ENTIRE process (agent + MCP servers) inside one.
	// This eliminates the split-brain where node_edit writes on the host but
	// exec runs in a container. Inside the container, --docker is cleared so
	// exec runs natively.
	dockerImg := resolveDockerImage(*docker)
	if dockerImg != "" && os.Getenv("DUN_DOCKER") == "" {
		// Full Docker mode: re-exec the entire process (agent + MCP servers)
		// inside a container. This eliminates the split-brain where node_edit
		// writes on the host but exec runs in a separate container.
		//
		// When dockerImg is the bundled image (dun:local), we build it from
		// the Dockerfile if it doesn't exist yet. Any other image cannot run
		// the host dun binary (it is not mounted, and is cgo), so it is
		// refused: /isolate docker image uses a custom image for exec only.
		if err := checkDockerFlag(dockerImg); err != nil {
			fmt.Fprintf(os.Stderr, "dun: --docker: %v\n", err)
			os.Exit(2)
		}
		if dockerImg == defaultDockerImage {
			dockerImg = "dun:local"
			if err := ensureDunImage(); err != nil {
				fmt.Fprintf(os.Stderr, "dun: --docker: %v\n", err)
				fmt.Fprintf(os.Stderr, "    Build it manually: tools/build-docker.sh\n")
				os.Exit(1)
			}
		}

		selfPath, err := os.Executable()
		if err != nil {
			selfPath = "dun"
		}
		absWorkspace, err := filepath.Abs(*ws)
		if err != nil {
			fatal(err)
		}
		args := []string{
			"run", "-i", "--rm",
			"-v", absWorkspace + ":/work", "-w", "/work",
			"-e", "DUN_DOCKER=1",
		}
		if *dockerNetwork {
			args = append(args, "--network", "host")
		} else {
			args = append(args, "--network", "none")
		}
		// Use the baked-in binary for our bundled image; for custom images,
		// the host binary may be inside the mounted workspace and would
		// resolve to a stale or wrong copy inside the container.
		if dockerImg == "dun:local" {
			// dun:local has ENTRYPOINT ["dun"], so just pass the args
			args = append(args, dockerImg)
		} else {
			dunInContainer := selfPath
			args = append(args, dockerImg, dunInContainer)
		}
		args = append(args, dockerReexecArgs()...)
		cmd := exec.CommandContext(context.Background(), "docker", args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			fatal(fmt.Errorf("docker run: %w", err))
		}
		os.Exit(cmd.ProcessState.ExitCode())
	}
	if !*prog {
		warnShipRemoved()
	}
	firstTask := strings.TrimSpace(strings.Join(flag.Args(), " "))
	// dun -p "task": a one-shot whose answer alone goes to stdout (answer.go);
	// dun -p with no task stays the JSON engine.
	answerMode := *prog && firstTask != ""
	if answerMode {
		*prog = false
	}

	if len(media) > 0 && (*prog || firstTask == "" || *tuiFlag || *serve) {
		fatal(fmt.Errorf("--media attaches to a one-shot task: dun --media shot.png \"what is wrong here\""))
	}

	if *ver {
		fmt.Println("dun " + buildinfo.Version)
		return
	}
	if *setup {
		if !haveTTY() {
			fmt.Fprint(os.Stderr, setupNoTTY)
			os.Exit(2)
		}
		if err := tui.RunSetupTUI(); err != nil {
			fatal(err)
		}
		return
	}
	// Every mode, not just the TUI: the engine is where a turn burns CPU, and
	// it is the process a TUI session would want to profile. Opt-in, loopback.
	startPprof()
	suggestEnabled = !*noSuggest // the UI may ASK for next-message suggestions (on by default)
	enginecmd.RephraseOn = *rephrase
	if !suggestEnabled {
		enginecmd.SuggestMode = "off"
	}
	engineModel, engineURL = *model, *url
	if p, ok := fc.ProviderAt(*url); ok {
		engineProvider = p.Name
	}
	// Resolve the effective key: explicit flag > env > saved config.
	// The saved key is the one of the provider at --url, so a --url naming
	// another configured provider gets that provider's key.
	savedKey := fc.Key
	if p, ok := fc.ProviderAt(*url); ok {
		savedKey = p.Key
	}
	effKey := dunconfig.FirstNonEmpty(*key, os.Getenv("DUN_LLM_KEY"), savedKey)
	// Dev self-update: if this is a source-stamped build and the tree changed,
	// rebuild in place and re-exec the fresh binary. Skipped for spawned children
	// (DUN_CHILD) and the -p engine mode; no-op for released binaries (srcDir="").
	if !*prog {
		selfUpdate()
	}

	absWS, err := filepath.Abs(*ws)
	if err != nil {
		fatal(err)
	}
	// --no-dir: dun's own directory for such sessions, not this one
	// (dun/nodir.go). A worktree or Docker needs a project to isolate.
	if *noDir {
		if *worktree || *docker != "" {
			fatal(fmt.Errorf("--no-dir has no project to put in a worktree or a container"))
		}
		if absWS, err = dun.NoDirWorkspace(); err != nil {
			fatal(err)
		}
	}

	if *listSessions {
		ids := dun.ListSessions(absWS)
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "dun: no saved sessions for this workspace")
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		return
	}

	// Replay: drive the TUI from a recorded trace instead of an engine. No LLM,
	// no subprocess, the same events with the same gaps — which is what makes a
	// UI performance number reproducible instead of anecdotal.
	if *replay != "" {
		pacing := tui.ReplayPacing{Speed: *replaySpeed, Delay: *inputDelay, MaxGap: *maxGap}
		if err := tui.RunReplay(*replay, pacing, tui.TuiOpts{Workspace: absWS, Model: *model, Url: *url}); err != nil {
			fatal(err)
		}
		return
	}

	// TUI mode: a Bubble Tea client of `dun -p` (re-exec'd with the same flags).
	//
	// The DEFAULT. `dun` on its own used to print a usage line and exit, which
	// made the interactive UI — the thing you want nine times out of ten — the
	// one mode you had to ask for. A positional task still runs headless: that
	// is the scripting path and scripts must not suddenly open a UI.
	if *tuiFlag || (firstTask == "" && !*prog && !*serve) {
		if err := tui.RunTUI(tui.TuiOpts{Workspace: absWS, Model: *model, Url: *url, Key: effKey, Docker: resolveDockerImage(*docker), DockerNetwork: *dockerNetwork, Worktree: *worktree, Cont: *cont, Resume: *resume, DisableExit: *disableExit, Suggest: !*noSuggest, Rag: ragFlag.String(), Lsp: lspFlag.String(), Rephrase: *rephrase, Fresh: *fresh, NoDir: *noDir, NoTools: *noTools, ItermTabs: *itermTabs}); err != nil {
			fatal(err)
		}
		return
	}

	// Serve mode: the TUI over xterm.js; each browser tab spawns a web session.
	if *serve {
		if err := runServe(tui.TuiOpts{Workspace: absWS, Model: *model, Url: *url, Key: effKey, Docker: resolveDockerImage(*docker), DockerNetwork: *dockerNetwork, Worktree: *worktree, Cont: *cont, Resume: *resume, DisableExit: *disableExit, Suggest: !*noSuggest, Rag: ragFlag.String(), Lsp: lspFlag.String(), Rephrase: *rephrase, Fresh: *fresh, NoDir: *noDir, NoTools: *noTools}, *addr); err != nil {
			fatal(err)
		}
		return
	}

	// A headless task with nothing to send to stops here, before servers,
	// worktrees or a session: /login cannot be typed here (nologin.go).
	if !*prog {
		if err := dunconfig.ProviderMissing(*url, *model); err != nil {
			fmt.Fprint(os.Stderr, headlessMissing(err))
			os.Exit(2)
		}
	}

	// Session persistence, scoped by the workspace ROOT (~/.dun/sessions/<root>/).
	var sessionFile, sessionID string
	switch {
	case *resume != "":
		sessionID, sessionFile = *resume, dun.SessionFile(absWS, *resume)
	case *cont:
		if sessionID = dun.LatestSession(absWS); sessionID != "" {
			sessionFile = dun.SessionFile(absWS, sessionID)
		}
	}
	if sessionFile == "" {
		sessionFile, sessionID = dun.NewSessionFile(absWS)
	}
	// Every command dun runs knows the session it runs in: dun session info
	// with no id reads it (sessioninfo.go).
	_ = os.Setenv("DUN_SESSION_ID", sessionID)
	// No per-session raglit home. It used to be an os.MkdirTemp deleted on exit,
	// which re-embedded the whole workspace every session and discarded it —
	// and made a durable index impossible by construction. raglit now resolves
	// its own home and namespaces this workspace by PROJECT (see
	// dun.DefaultServers), so the index outlives the session and several
	// sessions share one.

	// Register the workspace as a project on first sight, importing what an
	// older dun left in its .dun/. Everything dun keeps about it lives in
	// $DUN_HOME/projects/<id>/, never in the repo. Not fatal: without it dun
	// still reads the repo's own config, it only cannot save anything.
	if *noDir {
		// not a project: nothing to register
	} else if _, err := dun.EnsureProject(absWS); err != nil {
		fmt.Fprintf(os.Stderr, "dun: %v\n", err)
	}

	// Resolve extra mounts: local paths that must be accessible from both
	// the worktree (symlink) and the Docker container (volume mount).
	// Loaded from dun.json / dun.local.json and auto-discovered from go.mod.
	mounts := dun.LoadMounts(absWS, absWS)

	// Isolation tier 1: a git worktree (with --worktree). The agent's file
	// changes land here on a fresh branch, not on the checked-out branch.
	// Mounts are symlinked into the worktree parent so replace directives resolve.
	// When resuming, try to reuse the previous session's worktree so file edits
	// are preserved across sessions.
	effWS := absWS
	var wt *dun.Worktree
	if *worktree {
		var w *dun.Worktree
		var isRepo bool
		var werr error
		if sessionID != "" {
			// Resuming — try to reuse the old worktree
			meta := dun.LoadSessionMeta(absWS, sessionID)
			w, isRepo, werr = dun.ReuseWorktree(absWS, meta.WorktreePath, meta.Branch, mounts)
		}
		if w == nil {
			w, isRepo, werr = dun.NewWorktree(absWS, mounts)
		}
		if werr != nil {
			fatal(werr)
		}
		wt, effWS = w, w.Path
		// Written NOW, not on the way out. This used to be a deferred call in
		// main, so it only ran on a clean exit — which is the one path that does
		// not happen when the TUI kills its engine or you ctrl+C. Measured: 57
		// sessions on this machine, 0 metadata files, so --resume could never
		// reuse a worktree and nothing could tell which tree belonged to what.
		// There is nothing to learn by waiting: the path and branch are known
		// the moment the worktree exists.
		if sessionID != "" {
			_ = dun.SaveSessionMeta(absWS, sessionID, dun.SessionMeta{WorktreePath: wt.Path, Branch: wt.Branch})
		}
		if !isRepo && !*prog {
			fmt.Fprintf(os.Stderr, "dun: %s is not a git repo — working in place (no isolation)\n", absWS)
		}
	} else {
		// Without --worktree: a pass-through worktree on the checked-out
		// directory, so branch and status are known.
		wt, _ = dun.WorktreeInPlace(absWS)
	}

	// Housekeeping, once the live worktree is known so it is never a candidate:
	// remove session trees that hold nothing, drop registrations whose directory
	// is gone, and bound the session history. Anything holding work is reported,
	// never removed — a branch with unpushed commits may exist nowhere else.
	if repoRoot := dun.RepoRoot(absWS); repoRoot != "" {
		live := ""
		if wt != nil {
			live = wt.Path
		}
		res := dun.PruneWorktrees(repoRoot, live)
		gone := dun.PruneSessions(absWS, repoRoot, sessionID)
		if !*prog {
			reportHousekeeping(res, gone)
		}
	}

	// Isolation tier 2: exec runs in a Docker container (--docker IMAGE), or host.
	// --docker=true resolves to the default image (ubuntu:24.04).
	dockerImage := resolveDockerImage(*docker)
	var backend dun.ExecBackend
	if dockerImage != "" {
		backend = dun.DockerExec{Dir: effWS, Image: dockerImage, Network: *dockerNetwork, ExtraMounts: mounts, TmpDir: dun.MkIsoTmp(sessionFile)}
	} else {
		// Persistent shell: exports survive between exec calls, while the
		// working directory resets to the project root each time.
		backend = &dun.HostShell{Dir: effWS}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// --timeout bounds a TURN when the engine is interactive, and the WHOLE RUN
	// when it is one-shot.
	//
	// It used to bound the run either way, which quietly made the session
	// unrecoverable at the 30-minute mark: the running turn died with "context
	// deadline exceeded", every following turn failed instantly on the same dead
	// context, and the reader loop's ctx.Done() case exited the engine — so the
	// TUI's "send a message to retry" was advice that could not work. A wall
	// clock on a session a human is sitting in front of was the wrong thing to
	// measure; a turn that has hung is the thing worth cutting off.
	// Either way the budget is a pausable clock, not a context deadline: time a
	// human spends answering ask_user is not dun working, and must not be
	// charged to dun's budget (see turnclock.go).
	turnGrace = *timeoutGrace
	if *prog {
		go refreshRegistry(ctx) // for the next start
		turnTimeout = *timeout
	} else {
		run := newTurnClock(ctx, *timeout, graceFor(*timeout))
		curClock.Store(run)
		ctx = run.ctx
		defer run.Stop()
	}
	// The Shaper's compaction must not burn the turn's budget clock (dun
	// package, wired from here because the clock is a cmd/dun concern). In
	// one-shot mode curClock is the run's clock and pauseClock is still the
	// right thing to wrap the fold with; in interactive mode it's the running
	// turn's clock.
	dun.CompactionPause = pauseClock

	// started holds the Harness once dun.Start returns, for the callbacks that
	// are installed on cfg before it exists. atomic because the breakdown
	// measurement runs on its own goroutine.
	var started atomic.Value
	var em *emitter
	var in *inputStream
	client := llm.NewClient(*url, effKey, *model)
	tuneClient(fc, client, *url) // its /effort dialect and retries; the env wins, below
	// Logged out, or no model chosen: a stand-in that refuses every call with
	// what to do, so the engine still starts (a client can /login through it)
	// and nothing is contacted.
	// Money and limits: every response from this client and from every client
	// the per-model cache builds reports into one meter, so the session's spend
	// covers the turn, its side calls and its sub-agents. See dun/spend.go.
	spend := spendmeter.NewMeter()
	// Every response with a cost is also a line in the machine's spend ledger
	// ($DUN_HOME/spend, dun/ledger.go), billed to the provider, a fingerprint
	// of the key (never the key) and the model.
	spend.SetLedger(spendmeter.NewLedger(dun.DefaultLedgerDir()), spendmeter.KeyFingerprint(effKey))
	usageFor := func(u, k, model, role string) func(llm.Usage) {
		return spend.NoteAs(spendmeter.Source{Provider: providerLabel(fc, u), Key: spendmeter.KeyFingerprint(k), Model: model, Role: role})
	}
	client.OnUsage = usageFor(*url, effKey, *model, spendmeter.RoleMain)
	var runner agent.LLMRunner = client
	childClients := clientCache(fc, *url, effKey, func(c *llm.Client, u string) { tuneClient(fc, c, u) },
		func(u, k, model string) func(llm.Usage) { return usageFor(u, k, model, spendmeter.RoleModel) })
	if why := dunconfig.ProviderMissing(*url, *model); why != nil {
		runner = noProvider{why}
		childClients = func(string, string) (dun.ChildClient, error) { return dun.ChildClient{}, why }
	}
	var providerNames []string
	for _, p := range fc.ProviderList() {
		providerNames = append(providerNames, p.Name)
	}
	cfg := dun.Config{
		// The context window set for this model is filed under it, and an
		// OpenAI-compatible model list is asked with it (dun/window.go).
		Endpoint: dun.Endpoint{URL: *url, Key: effKey, Model: *model},
		// What the harness tool and dun session info say (dun/harnessinfo.go).
		Version: buildinfo.Version, Provider: engineProvider,
		Workspace: effWS,
		Client:    runner,
		// Sub-agents may run on a different model than their parent, and only
		// cmd/dun knows the endpoint and the key — hence a factory rather than a
		// second client. Cached per model so N children on one model share one
		// client, and therefore one retry policy and one rate limit.
		ClientFor: childClients,
		Providers: providerNames,
		Spend:     spend,
		// Each round's tokens, time and cost, for the turn's stats and what a
		// tool server costs (dun/roundstats.go).
		StatsLedger:     dun.NewStatsLedger(dun.DefaultStatsDir()),
		ChildModel:      *childModel,
		ChildProvider:   *childProvider,
		ChildEffort:     *childEffort,
		Exec:            backend,
		Worktree:        wt,
		CommitCfg:       dun.LoadCommit(absWS),
		Files:           filesConfig(absWS, *noDir),
		NoDir:           *noDir,
		NoTools:         *noTools,
		NonInteractive:  *nonInteractive && *prog,
		TurnLimit:       *turnLimit,
		AsideEscalate:   time.Duration(*asideEscalateMS) * time.Millisecond,
		SettleDelay:     settleDelayFlag(*settleDelay),
		PlanNudgeRounds: intFlagOff("plan-nudge-rounds", *planNudgeRounds),
		PlanNudgeFails:  intFlagOff("plan-nudge-fails", *planNudgeFails),
		PlanStallRounds: intFlagOff("plan-stall-rounds", *planStallRounds),
		EffortOff:       effortOff(fc, *url),
		SessionFile:     sessionFile,
		Persist:         *persistFlag,
		// DUN_MEMORY_TRIGGERS=0: no memory fork unless asked (/remember, the
		// memory tool). See memtrigger.go.
		DisableMemoryTriggers: os.Getenv("DUN_MEMORY_TRIGGERS") == "0",
		// The workspace's own checkout, not the worktree: /rag auto is a fact
		// about this machine and this project, and must outlive the throwaway
		// worktree this session works in.
		ConfigDir:         absWS,
		AutostartOverride: autostartOverrides(ragFlag, lspFlag),
		// Stored so /isolate can rehoist with the right
		// image and mounts without the TUI knowing about them.
		DockerImage:   resolveDockerImage(*docker),
		DockerNetwork: *dockerNetwork,
		ExtraMounts:   mounts,
		Vision:        visionConfig(fc, *url, effKey, *model, func(u, k, m string) func(llm.Usage) { return usageFor(u, k, m, spendmeter.RoleVision) }),
	}
	// An OAuth server's login opens a browser on this machine too — unless
	// DUN_NO_BROWSER is set (a headless box, a test). The URL is shown to
	// the person either way.
	if os.Getenv("DUN_NO_BROWSER") == "" {
		cfg.OpenBrowser = openurl.Open
	}
	var peers *peerLink
	if *prog {
		em = &emitter{}
		in = newInputStream()
		// Under dund, the model can see and message the other live sessions
		// (peers.go); standalone, the tools are absent.
		if underDund() {
			peers = newPeerLink(em)
			cfg.Peers = peers
			cfg.Loops = peers
			cfg.OAuthBroker = peers.oauth // dund holds the OAuth tokens
			cfg.Gateway = peers           // and the shared MCP servers' connections
		}
		cfg.OnToken = func(s string) { em.emit(&proto.Token{Text: s}) }
		cfg.OnReasoning = func(s string) { em.emit(&proto.Reasoning{Text: s}) }
		cfg.OnRequest = func(r dun.RequestStat) { em.emit(requestEvent(r)) }
		// An autostart server that came up (or did not) after the session
		// was ready. Before Start returns there is no harness to describe;
		// the ready event that follows carries the server list instead.
		cfg.OnServerStarted = func(id string, err error) {
			h, _ := started.Load().(*dun.Harness)
			if h == nil {
				return
			}
			msg := id + ": " + runningSummary(h, id)
			var miss *dun.MissingError
			switch {
			case errors.As(err, &miss): // a setup step, said quietly
				msg = id + " off: " + textfmt.OneLine(err.Error())
			case err != nil:
				msg = id + ": did not start — " + textfmt.OneLine(err.Error())
			}
			em.emit(&proto.ServerEvent{ID: id, Action: "start", Message: msg,
				Servers: enginecmd.ServersProto(h.Servers()), Tools: h.ToolNames(), Background: true})
			em.emit(catalogEvent(h))
			em.emit(commandsEvent(h))
		}
		// A line in a server's activity log (dun/serverlog.go): the server
		// list again, so a client shows a start while it is in flight. A
		// start writes many lines at once (its stderr): one event per
		// activityEvery at most.
		var actMu sync.Mutex
		actDue := false
		cfg.OnServerActivity = func(string) {
			actMu.Lock()
			defer actMu.Unlock()
			if actDue {
				return
			}
			actDue = true
			time.AfterFunc(activityEvery, func() {
				actMu.Lock()
				actDue = false
				actMu.Unlock()
				if h, _ := started.Load().(*dun.Harness); h != nil {
					em.emit(&proto.ServerEvent{Action: "activity", Servers: enginecmd.ServersProto(h.Servers()),
						Tools: h.ToolNames(), Background: true})
				}
			})
		}
		// A server's index build ended (dun/indexstate.go): the new state,
		// and what dun did about it when it did something.
		cfg.OnServerIndex = func(id, msg string) {
			h, _ := started.Load().(*dun.Harness)
			if h == nil {
				return
			}
			em.emit(&proto.ServerEvent{ID: id, Action: "index", Message: msg,
				Servers: enginecmd.ServersProto(h.Servers()), Tools: h.ToolNames(), Background: true})
		}
		// The task plan changed (dun/plan.go): a screen reads it again.
		cfg.OnPlan = func() {
			if h, _ := started.Load().(*dun.Harness); h != nil {
				em.emit(planEvent(h, ""))
			}
		}
		// An install hint, once (dun/setupstate.go): said as a quiet line.
		// It is said once, so a Harness not yet stored is waited for.
		cfg.OnServerSetup = func(id, msg string) {
			go func() {
				for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
					if h, _ := started.Load().(*dun.Harness); h != nil {
						em.emit(&proto.ServerEvent{ID: id, Action: "setup", Message: msg,
							Servers: enginecmd.ServersProto(h.Servers()), Tools: h.ToolNames(), Background: true})
						return
					}
				}
			}()
		}
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			em.emit(&proto.ToolCall{Tool: tool, Args: args})
			em.emit(&proto.ToolResult{Tool: tool, Result: result})
		}
		cfg.OnNotify = func(text string) { em.emit(&proto.Notification{Text: text}) }
		cfg.OnLogin = func(server, url string) { em.emit(&proto.McpLogin{Server: server, URL: url}) }
		// A memory trigger reports like /remember does, so the UI shows it the
		// same way.
		cfg.OnMemoryTrigger = func(msg string) {
			em.emit(&proto.ControlEvent{ID: "remember", Action: "trigger", Message: msg})
		}
		cfg.OnRules = func(msg string) { em.emit(&proto.ControlEvent{ID: "rules", Action: "offer", Message: msg}) }
		// The whole list every time, not a delta: it is a handful of rows, and a
		// UI that has to reconstruct state from deltas gets it wrong the first
		// time an event is dropped.
		cfg.OnAgents = func(as []dun.AgentInfo) { em.emit(&proto.Agents{Agents: agentsProto(as)}) }
		cfg.OnAgentEntry = func(id int, items []dun.HistoryItem) {
			em.emit(&proto.AgentEntry{Agent: id, Items: historyProto(items)})
		}
		cfg.OnJobs = func(js []dun.JobInfo) { em.emit(&proto.Jobs{Jobs: jobsProto(js)}) }
		// started is filled in once dun.Start returns. The breakdown callback is
		// installed BEFORE the Harness exists and fires after it does, and the
		// window division it wants to carry can only come from the Harness — so
		// the reference is late-bound rather than captured. Nil-checked because
		// an estimate can land before the assignment on a slow start.
		cfg.OnSystemBreakdown = func(bd dun.SystemBreakdown) {
			ev := &proto.ContextCost{SystemCost: systemCost(bd, bd.Total)}
			if hp, _ := started.Load().(*dun.Harness); hp != nil {
				w := windowOf(hp.Window())
				ev.Window = &w
			}
			em.emit(ev)
		}
		cfg.OnDocs = func(n dun.DocsNote) {
			em.emit(&proto.Notification{Kind: "docs", Found: n.Found, Surfaced: n.Surfaced, Docs: docsProto(n.Docs),
				Bytes: n.Bytes, Tokens: n.Tokens, LatencyMS: n.LatMS})
		}
		cfg.OnRetry = func(n dun.RetryNote) { em.emit(retryEvent(n)) }
		// The count, the citation and what REPLACED the span — never the removed
		// content itself: re-rendering the churn a recap just took out would
		// defeat the point of it. Recap no longer asks first, so this event is
		// the only place a human learns it happened; it carries enough to judge
		// the rewrite after the fact.
		cfg.OnRecap = func(n dun.RecapNote) {
			em.emit(&proto.Recap{Entries: n.Entries, Chars: n.Chars, Note: n.Note, Detail: n.Detail})
		}
		cfg.OnCompaction = func(n dun.CompactionNote) {
			em.emit(&proto.Compaction{Text: n.String(), Subsumed: n.Subsumed,
				TokensBefore: n.TokensBefore, TokensAfter: n.TokensAfter,
				Turn: n.Turn, SinceLastSecs: n.SinceLastSecs, Cause: n.Cause})
		}
		// askMu: one question open at a time. Every question (the model's
		// ask_user, a mount, dun's own "not a project") reads the next line of
		// in.answers; two open at once could each take the other's answer.
		var askMu sync.Mutex
		ask := func(actx context.Context, q string, opts []string, multi bool, rec []string) (string, error) {
			askMu.Lock()
			defer askMu.Unlock()
			// Paused for the whole wait: the person deciding is not the turn
			// working, and a question left open should never be what kills the
			// turn that asked it.
			return withoutClock(func() (string, error) {
				ask := &proto.Ask{Question: q, Options: opts, Multi: multi, RecommendedAll: rec}
				if len(rec) > 0 {
					ask.Recommended = rec[0]
				}
				em.emit(ask)
				select {
				case a, ok := <-in.answers:
					if !ok {
						return "", fmt.Errorf("input closed")
					}
					em.emit(&proto.Answered{Value: a})
					return a, nil
				case <-actx.Done():
					return "", actx.Err()
				}
			})
		}
		// --non-interactive: nobody answers, so no ask_user (a nil Ask offers none).
		if !cfg.NonInteractive {
			cfg.Ask = ask
		}
		cfg.Mount = func(actx context.Context, source, name string) (bool, error) {
			askMu.Lock()
			defer askMu.Unlock()
			return withoutClock(func() (bool, error) {
				em.emit(&proto.Mount{Source: source, Name: name})
				select {
				case a, ok := <-in.answers:
					if !ok {
						return false, fmt.Errorf("input closed")
					}
					em.emit(&proto.Answered{Value: a})
					return strings.TrimSpace(strings.ToLower(a)) == "yes" || strings.TrimSpace(strings.ToLower(a)) == "y", nil
				case <-actx.Done():
					return false, actx.Err()
				}
			})
		}
	} else {
		cfg.OnToken = func(s string) { fmt.Print(s) }
		if answerMode {
			cfg.OnToken = func(s string) { fmt.Fprint(os.Stderr, s) } // the work; the answer is printed after
			cfg.SystemNote = answerSystem
		}
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			fmt.Fprintf(os.Stderr, "\n  ⚙ %s(%s) → %s\n", tool, shortArgs(args), textfmt.Clip(textfmt.OneLine(result), 200))
		}
		cfg.OnNotify = func(text string) { fmt.Fprintf(os.Stderr, "\n  🔔 %s\n", textfmt.Clip(textfmt.OneLine(text), 200)) }
		cfg.OnLogin = func(server, url string) { fmt.Fprintf(os.Stderr, "\n  🔑 %s\n", enginecmd.LoginLine(server, url)) }
		cfg.OnMemoryTrigger = func(msg string) { fmt.Fprintf(os.Stderr, "\n  🧠 %s\n", textfmt.Clip(textfmt.OneLine(msg), 200)) }
		cfg.OnRules = func(msg string) { fmt.Fprintf(os.Stderr, "\n  %s\n", msg) }
		cfg.OnDocs = func(n dun.DocsNote) {
			fmt.Fprintf(os.Stderr, "\n  🔎 %d relevant doc(s) · %d surfaced\n", n.Found, n.Surfaced)
			for _, d := range n.Docs {
				fmt.Fprintf(os.Stderr, "     %s  (%s)\n       %s\n", d.Title, d.Why(), d.Source())
			}
		}
		cfg.OnRetry = func(n dun.RetryNote) { fmt.Fprintf(os.Stderr, "\n  %s %s\n", retryGlyph(n), n.String()) }
		cfg.OnCompaction = func(n dun.CompactionNote) { fmt.Fprintf(os.Stderr, "\n  🗜 %s\n", n) }
		cfg.Ask = func(actx context.Context, q string, opts []string, multi bool, rec []string) (string, error) {
			return withoutClock(func() (string, error) { return humanAsk(actx, q, opts, multi, rec) })
		}
		cfg.Mount = func(actx context.Context, source, name string) (bool, error) {
			return withoutClock(func() (bool, error) {
				ans, err := humanAsk(actx,
					fmt.Sprintf("Mount %s as /%s inside the container?", source, name),
					[]string{"yes", "no"}, false, nil)
				if err != nil {
					return false, err
				}
				return strings.TrimSpace(strings.ToLower(ans)) == "yes", nil
			})
		}
		fmt.Fprintf(os.Stderr, "dun: spawning tool servers for %s …\n", absWS)
	}

	dropKeyEnv(fc, effKey)
	h, err := dun.Start(ctx, cfg)
	if h != nil {
		started.Store(h)
		enginecmd.EngineHarness.Store(h)
		// The budget's two marks (turnclock.go): tell the model, then cut.
		budgetHooks.Store(&budgetMarks{
			spent: func(budget, grace time.Duration) {
				h.NoteBudget(budget, grace)
				msg := fmt.Sprintf("turn budget %s spent — the model is told to wrap up; cut in %s",
					dun.BudgetDur(budget), dun.BudgetDur(grace))
				if em != nil {
					em.emit(&proto.Notification{Kind: "budget", Text: msg})
				} else {
					fmt.Fprintf(os.Stderr, "\ndun: %s\n", msg)
				}
			},
			cut: h.NoteBudgetCut,
		})
	}
	if err != nil {
		if em != nil {
			// Fatal: there is no session yet, so "send a message to retry" would
			// be advice with nothing to send it to.
			em.emit(&proto.Error{Error: err.Error(), Fatal: true})
		}
		fatal(err)
	}
	defer h.Close()

	if *prog {
		em.setSession(sessionID)
		em.emit(&proto.Session{ID: sessionID, Resumed: h.Resumed()})
		// Replay the loaded conversation so a resuming client rebuilds scrollback
		// (the model already holds it as context; this is pure display).
		if items := h.History(); len(items) > 0 {
			em.emit(&proto.History{Items: historyProto(items)})
		}
	} else if h.Resumed() > 0 {
		fmt.Fprintf(os.Stderr, "dun: resumed session %s (%d entries)\n", sessionID, h.Resumed())
	} else {
		fmt.Fprintf(os.Stderr, "dun: session %s\n", sessionID)
	}

	if wt != nil && wt.Branch != "" {
		if *prog {
			em.emit(&proto.Workspace{Path: effWS, Branch: wt.Branch})
		} else {
			fmt.Fprintf(os.Stderr, "dun: worktree %s (branch %s)\n", effWS, wt.Branch)
		}
	}

	// The last known spend, before any request and before the budget read
	// below: the machine's ledger, the key's last budget, and this month's
	// usage (the splash's spend row).
	if *prog {
		sp := spendProto(h.Spend(), time.Now())
		sp.Month = monthUsage(h, time.Now())
		em.emit(sp)
	}

	// What /effort default does on this server, when its model list says
	// (corrallm's thinking_* fields): the settings and the command list
	// again once it is known.
	if *prog && dunconfig.ProviderMissing(*url, *model) == nil {
		go func() {
			if h.LearnServerThinking(ctx) {
				em.emit(settingsEvent())
				em.emit(commandsEvent(h))
			}
		}()
	}

	// The key's budget: read at start, every few minutes and after a turn, on
	// its own goroutine — never on the turn path. An endpoint with none stops
	// the poller after one probe per documented path.
	if dunconfig.ProviderMissing(*url, *model) == nil {
		go spend.PollBudget(ctx, client, spendmeter.BudgetPollEvery, spendmeter.BudgetPollGap, func() {
			if em != nil {
				em.emit(spendProto(h.Spend(), time.Now()))
			}
		})
	}

	if *prog {
		runProgrammatic(ctx, h, em, in, peers, firstTask, closeCtx{workspace: absWS, sessionID: sessionID, wt: wt})
		return
	}
	reply := runHuman(ctx, h, firstTask, absWS, media.inputs(), *noTools)
	if answerMode {
		fmt.Println(answerOf(reply))
	}

	// Report the changes the agent made in the isolated worktree.
	if wt != nil && wt.Branch != "" {
		if d := strings.TrimSpace(wt.Diff()); d != "" {
			fmt.Fprintf(os.Stderr, "\ndun: changes on branch %s (worktree %s):\n%s\n", wt.Branch, effWS, textfmt.Clip(d, 4000))
		} else {
			fmt.Fprintf(os.Stderr, "\ndun: no file changes. remove the worktree with: git worktree remove %s\n", effWS)
		}
	}
}

// runProgrammatic drives dun over line-delimited JSON events. Input is read by
// the inputStream's goroutine (so an `ask` inside a turn can consume `answer`
// events while this loop is blocked in a turn); this loop just handles `user`
// messages.
//
// Two ways the input ends, and they mean different things:
//
//   - EOF (a one-shot `dun -p 'task'` with nothing on stdin): the caller has
//     nothing more to say, so DRAIN — keep running turns until the background
//     jobs the agent started have reported in. Returning early would exit with
//     those jobs still in flight and their notifications never delivered.
//   - An explicit stop/quit event: the caller asked to be let go NOW. Return
//     without waiting on anything.
//
// closeCtx is what /close needs and the harness cannot supply: which workspace
// and which session this engine is serving, plus the worktree it made.
type closeCtx struct {
	workspace string
	sessionID string
	wt        *dun.Worktree
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "dun: %v\n", err)
	os.Exit(1)
}

// filesConfig is the workspace's file-tool settings; none for a --no-dir
// session, which has no project files (dun/nodir.go).
func filesConfig(ws string, noDir bool) *dun.FilesConfig {
	if noDir {
		return &dun.FilesConfig{Tools: []string{}}
	}
	return dun.LoadFiles(ws)
}

// intFlagOff is an int flag for a Config field where 0 means "unset" and a
// negative value is off: a flag given as 0 is off (dun/plannudge.go).
func intFlagOff(name string, v int) int {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	if set && v == 0 {
		return -1
	}
	return v
}

// settleDelayFlag turns --settle-delay into Config.SettleDelay, where 0 means
// "unset" and a negative value is off: a flag given as 0 is off (dun/settle.go).
func settleDelayFlag(d time.Duration) time.Duration {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == "settle-delay" })
	if set && d == 0 {
		return -1
	}
	return d
}
