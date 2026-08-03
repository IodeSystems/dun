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
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

// version is stamped at build time via -ldflags "-X main.version=…" (see the
// Makefile). "dev" for a plain `go build`. Shown by `dun -version` and in the
// TUI header so a stale on-PATH binary is visible at a glance.
var version = "dev"

// usage prints the flags as --long-flags.
//
// Go's flag package accepts -tui and --tui identically but PRINTS the first,
// and `-tui` reads like the three short flags -t -u -i. Only genuinely
// single-letter flags keep one dash.
func usage() {
	fmt.Fprint(os.Stderr, `dun — a coding agent that works a task in an isolated workspace.

usage:
  dun                        interactive TUI in the current directory (the default)
  dun "fix the flaky test"   run one task headless, print the diff, exit
  dun --continue             reopen the most recent session for this workspace
  dun -p                     engine mode: line-delimited JSON events on stdin/stdout

flags:
`)
	flag.VisitAll(func(f *flag.Flag) {
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
		fmt.Fprintf(os.Stderr, "%-24s %s\n", head, help)
	})
}

func main() {
	flag.Usage = usage
	ver := flag.Bool("version", false, "print version and exit")
	setup := flag.Bool("setup", false, "run the interactive setup wizard (LLM url/model/key) and exit")
	// Flag defaults come from env then the saved config then the built-in — so a
	// CLI flag still overrides for a one-off run. See config.go.
	fc := loadConfig()
	url := flag.String("url", firstNonEmpty(os.Getenv("DUN_URL"), fc.URL, defaultURL), "LLM base URL")
	model := flag.String("model", firstNonEmpty(os.Getenv("DUN_MODEL"), fc.Model, defaultModel), "chat model (must support tool calls)")
	// Key default stays empty so `dun -h` never prints the secret; the real value
	// is resolved after parse (flag > env > config).
	key := flag.String("key", "", "API key (set via $DUN_LLM_KEY or 'dun --setup')")
	ws := flag.String("workspace", ".", "workspace directory (a git repo → worktree isolation)")
	docker := flag.String("docker", "", "run exec commands in a Docker container of this image (empty = host)")
	worktree := flag.Bool("worktree", false, "create a git worktree for isolation (default: work in place)")
	childModel := flag.String("child-model", "", "model for spawned sub-agents (default: same as --model)")
	pr := flag.Bool("pr", false, "shorthand for --ship with mode pr (verify, push, then report the gh pr create command)")
	// Ship is ON by default: an agent that finishes work and cannot land it has
	// done half a job, and the modes in ship.allow are the real policy surface —
	// a repo that does not want pushes says so there, not by withholding the
	// tool. --no-ship is the opt-out for a session that should only ever look.
	shipFlag := flag.Bool("ship", true, "let the agent ship work (fetch+rebase+checks+push) when done")
	noShip := flag.Bool("no-ship", false, "withhold the ship tool entirely (overrides --ship)")
	cont := flag.Bool("continue", false, "resume the most recent session for this workspace")
	resume := flag.String("resume", "", "resume a specific session id (see --sessions)")
	listSessions := flag.Bool("sessions", false, "list saved sessions for this workspace and exit")
	prog := flag.Bool("p", false, "programmatic mode: emit + read line-delimited JSON events")
	tui := flag.Bool("tui", false, "launch the interactive Bubble Tea UI")
	serve := flag.Bool("serve", false, "serve the TUI over the web (xterm.js) at --addr")
	addr := flag.String("addr", "127.0.0.1:8734", "serve: HTTP listen address")
	disableExit := flag.Bool("disable-exit", false, "TUI: ctrl+c / esc don't quit (exit via /exit)")
	suggest := flag.Bool("suggest", false, "after each turn, suggest likely next messages (one extra LLM call per turn)")
	daemon := flag.Bool("d", false, "run/query the launcher daemon: dun -d (run), dun -d status, dun -d shutdown")
	force := flag.Bool("force", false, "-d shutdown: proceed even with sessions attached")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout")
	replay := flag.String("replay", "", "replay a recorded session trace into the TUI (record one with DUN_TRACE=path)")
	replaySpeed := flag.Float64("replay-speed", 1, "replay: pacing multiplier over the recorded timing (2 = twice as fast)")
	inputDelay := flag.Duration("input-delay", -1, "replay: fixed gap between events, overriding the recording (0 = no delay); negative = use the recorded timing")
	maxGap := flag.Duration("max-gap", 2*time.Second, "replay: compress idle gaps longer than this (0 = replay the recording verbatim)")
	// Tri-state: unset means "whatever /rag auto or /lsp auto saved", which a
	// plain bool cannot express (its zero value would silently mean "off").
	var ragFlag, lspFlag tristate
	flag.Var(&ragFlag, "rag", "start the docs server (raglit) this run: --rag or --rag=false (default: the saved setting)")
	flag.Var(&lspFlag, "lsp", "start the code server (poly-lsp-mcp) this run: --lsp or --lsp=false (default: the saved setting)")
	flag.Parse()
	// --pr implies shipping: it is a ship MODE, not a separate ability.
	effShip := (*shipFlag && !*noShip) || *pr
	ship := &effShip
	firstTask := strings.TrimSpace(strings.Join(flag.Args(), " "))

	if *ver {
		fmt.Println("dun " + version)
		return
	}
	if *setup {
		if err := runSetupTUI(); err != nil {
			fatal(err)
		}
		return
	}
	// Launcher daemon (supervisor): `dun -d` runs it; `dun -d status|shutdown` query.
	if *daemon {
		switch firstTask {
		case "status":
			printLauncherStatus()
		case "shutdown":
			shutdownLauncher(*force)
		case "":
			if err := runLauncher(); err != nil {
				fatal(err)
			}
		default:
			fatal(fmt.Errorf("dun -d: unknown subcommand %q (status|shutdown|<none>)", firstTask))
		}
		return
	}
	// Every mode, not just the TUI: the engine is where a turn burns CPU, and
	// it is the process a TUI session would want to profile. Opt-in, loopback.
	startPprof()
	suggestEnabled = *suggest // -p emits next-message suggestions after each turn
	// Resolve the effective key: explicit flag > env > saved config.
	effKey := firstNonEmpty(*key, os.Getenv("DUN_LLM_KEY"), fc.Key)
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
		pacing := replayPacing{speed: *replaySpeed, delay: *inputDelay, maxGap: *maxGap}
		if err := runReplay(*replay, pacing, tuiOpts{workspace: absWS, model: *model, url: *url}); err != nil {
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
	if *tui || (firstTask == "" && !*prog && !*serve) {
		lc := registerSession(selfKind(false), absWS) // supervisor registry + reload
		defer lc.close()
		if err := runTUI(tuiOpts{absWS, *model, *url, effKey, *docker, *worktree, *pr, *ship, *cont, *resume, *disableExit, *suggest, ragFlag.String(), lspFlag.String()}, lc); err != nil {
			fatal(err)
		}
		return
	}

	// Serve mode: the TUI over xterm.js; each browser tab spawns a web session.
	if *serve {
		lc := registerSession("serve", absWS)
		defer lc.close()
		if err := runServe(tuiOpts{absWS, *model, *url, effKey, *docker, *worktree, *pr, *ship, *cont, *resume, *disableExit, *suggest, ragFlag.String(), lspFlag.String()}, *addr); err != nil {
			fatal(err)
		}
		return
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
	// No per-session raglit home. It used to be an os.MkdirTemp deleted on exit,
	// which re-embedded the whole workspace every session and discarded it —
	// and made a durable index impossible by construction. raglit now resolves
	// its own home and namespaces this workspace by PROJECT (see
	// dun.DefaultServers), so the index outlives the session and several
	// sessions share one.

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
	} else if *ship || *pr {
		// --ship/--pr without --worktree: create a pass-through worktree so
		// the ship tool is available. The agent works in the checked-out
		// directory; ship's pipeline (rebase + checks) still runs.
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
	var backend dun.ExecBackend
	if *docker != "" {
		backend = dun.DockerExec{Dir: effWS, Image: *docker, ExtraMounts: mounts}
	} else {
		backend = dun.HostExec{Dir: effWS}
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
	if *prog {
		turnTimeout = *timeout
	} else {
		run := newTurnClock(ctx, *timeout)
		curClock.Store(run)
		ctx = run.ctx
		defer run.Stop()
	}

	var em *emitter
	var in *inputStream
	cfg := dun.Config{
		Workspace: effWS,
		Client:    llm.NewClient(*url, effKey, *model),
		// Sub-agents may run on a different model than their parent, and only
		// cmd/dun knows the endpoint and the key — hence a factory rather than a
		// second client. Cached per model so N children on one model share one
		// client, and therefore one retry policy and one rate limit.
		ClientFor:   clientCache(*url, effKey),
		ChildModel:  *childModel,
		Exec:        backend,
		Worktree:    wt,
		EnableShip:  *ship || *pr,
		ShipCfg:     shipConfig(absWS, *pr),
		SessionFile: sessionFile,
		// The workspace's own checkout, not the worktree: /rag auto is a fact
		// about this machine and this project, and must outlive the throwaway
		// worktree this session works in.
		ConfigDir:         absWS,
		AutostartOverride: autostartOverrides(ragFlag, lspFlag),
		// Stored so /docker on and /worktree new can rehoist with the right
		// image and mounts without the TUI knowing about them.
		DockerImage: *docker,
		ExtraMounts: mounts,
	}
	if *prog {
		em = &emitter{}
		in = newInputStream()
		cfg.OnToken = func(s string) { em.emit(event{"type": "token", "text": s}) }
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			em.emit(event{"type": "tool_call", "tool": tool, "args": args})
			em.emit(event{"type": "tool_result", "tool": tool, "result": result})
		}
		cfg.OnNotify = func(text string) { em.emit(event{"type": "notification", "text": text}) }
		// The whole list every time, not a delta: it is a handful of rows, and a
		// UI that has to reconstruct state from deltas gets it wrong the first
		// time an event is dropped.
		cfg.OnAgents = func(as []dun.AgentInfo) { em.emit(event{"type": "agents", "agents": agentsToAny(as)}) }
		cfg.OnJobs = func(js []dun.JobInfo) { em.emit(event{"type": "jobs", "jobs": jobsToAny(js)}) }
		cfg.OnDocs = func(n dun.DocsNote) {
			em.emit(event{"type": "notification", "kind": "docs", "found": n.Found, "surfaced": n.Surfaced, "docs": docsToAny(n.Docs)})
		}
		cfg.OnRetry = func(n dun.RetryNote) { em.emit(retryEvent(n)) }
		// The COUNT and the citation, never the removed content: re-rendering
		// the churn a recap just took out would defeat the point of it.
		cfg.OnRecap = func(n dun.RecapNote) {
			em.emit(event{"type": "recap", "entries": n.Entries, "chars": n.Chars, "note": n.Note})
		}
		cfg.OnCompaction = func(n dun.CompactionNote) {
			em.emit(event{"type": "compaction", "text": n.String(), "subsumed": n.Subsumed,
				"tokens_before": n.TokensBefore, "tokens_after": n.TokensAfter,
				"turn": n.Turn, "since_last_secs": n.SinceLastSecs})
		}
		cfg.Ask = func(actx context.Context, q string, opts []string, multi bool) (string, error) {
			// Paused for the whole wait: the person deciding is not the turn
			// working, and a question left open should never be what kills the
			// turn that asked it.
			return withoutClock(func() (string, error) {
				em.emit(event{"type": "ask", "question": q, "options": opts, "multi": multi})
				select {
				case a, ok := <-in.answers:
					if !ok {
						return "", fmt.Errorf("input closed")
					}
					return a, nil
				case <-actx.Done():
					return "", actx.Err()
				}
			})
		}
	} else {
		cfg.OnToken = func(s string) { fmt.Print(s) }
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			fmt.Fprintf(os.Stderr, "\n  ⚙ %s(%s) → %s\n", tool, shortArgs(args), clip(oneLine(result), 200))
		}
		cfg.OnNotify = func(text string) { fmt.Fprintf(os.Stderr, "\n  🔔 %s\n", clip(oneLine(text), 200)) }
		cfg.OnDocs = func(n dun.DocsNote) {
			fmt.Fprintf(os.Stderr, "\n  🔎 %d relevant doc(s) · %d surfaced\n", n.Found, n.Surfaced)
		}
		cfg.OnRetry = func(n dun.RetryNote) { fmt.Fprintf(os.Stderr, "\n  %s %s\n", retryGlyph(n), n.String()) }
		cfg.OnCompaction = func(n dun.CompactionNote) { fmt.Fprintf(os.Stderr, "\n  🗜 %s\n", n) }
		cfg.Ask = func(actx context.Context, q string, opts []string, multi bool) (string, error) {
			return withoutClock(func() (string, error) { return humanAsk(actx, q, opts, multi) })
		}
		fmt.Fprintf(os.Stderr, "dun: spawning tool servers for %s …\n", absWS)
	}

	h, err := dun.Start(ctx, cfg)
	if err != nil {
		if em != nil {
			// Fatal: there is no session yet, so "send a message to retry" would
			// be advice with nothing to send it to.
			em.emit(event{"type": "error", "error": err.Error(), "fatal": true})
		}
		fatal(err)
	}
	defer h.Close()

	if *prog {
		em.emit(event{"type": "session", "id": sessionID, "resumed": h.Resumed()})
		// Replay the loaded conversation so a resuming client rebuilds scrollback
		// (the model already holds it as context; this is pure display).
		if items := h.History(); len(items) > 0 {
			em.emit(event{"type": "history", "items": items})
		}
	} else if h.Resumed() > 0 {
		fmt.Fprintf(os.Stderr, "dun: resumed session %s (%d entries)\n", sessionID, h.Resumed())
	} else {
		fmt.Fprintf(os.Stderr, "dun: session %s\n", sessionID)
	}

	if wt != nil && wt.Branch != "" {
		if *prog {
			em.emit(event{"type": "workspace", "path": effWS, "branch": wt.Branch})
		} else {
			fmt.Fprintf(os.Stderr, "dun: worktree %s (branch %s)\n", effWS, wt.Branch)
		}
	}

	if *prog {
		runProgrammatic(ctx, h, em, in, firstTask, closeCtx{workspace: absWS, sessionID: sessionID, wt: wt})
		return
	}
	runHuman(ctx, h, firstTask, absWS)

	// Report the changes the agent made in the isolated worktree.
	if wt != nil && wt.Branch != "" {
		if d := strings.TrimSpace(wt.Diff()); d != "" {
			fmt.Fprintf(os.Stderr, "\ndun: changes on branch %s (worktree %s):\n%s\n", wt.Branch, effWS, clip(d, 4000))
		} else {
			fmt.Fprintf(os.Stderr, "\ndun: no file changes. remove the worktree with: git worktree remove %s\n", effWS)
		}
	}
}

// runHuman streams a single task, then drains any background jobs it started
// (their completion notifications trigger follow-up turns).
func runHuman(ctx context.Context, h *dun.Harness, task, workspace string) {
	fmt.Fprintf(os.Stderr, "dun: %d tools ready: %s\n",
		len(h.ToolNames()), strings.Join(h.ToolNames(), ", "))
	// Name what is NOT running. A one-shot run has no /rag to type, so point at
	// the flags instead of the commands.
	for _, st := range h.Servers() {
		switch {
		case st.Running:
		case st.Err != "":
			fmt.Fprintf(os.Stderr, "dun: %s did not start: %s\n", st.ID, oneLine(st.Err))
		default:
			fmt.Fprintf(os.Stderr, "dun: %s off (--%s to start it this run)\n", st.ID, aliasOf(st.ID))
		}
	}
	fmt.Fprintf(os.Stderr, "\ntask: %s\n\n", task)
	res, err := h.Ask(ctx, task)
	if err != nil {
		// The conversation is on disk, so this is recoverable — say how, since the
		// retries already visible above have run out and the alternative is a user
		// starting the whole task again.
		fmt.Fprintf(os.Stderr, "\ndun: resume with: dun --continue --workspace %s \"<what to do next>\"\n", workspace)
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\n\n--- done (%d tokens) ---\n", res.Usage.Total)

	for {
		if h.BackgroundRunning() == 0 {
			select {
			case <-h.Wake(): // a just-finished job's notification
			default:
				return
			}
		} else {
			select {
			case <-h.Wake():
			case <-ctx.Done():
				return
			}
		}
		fmt.Fprintf(os.Stderr, "\n--- background job finished; continuing ---\n")
		if _, err := h.Continue(ctx); err != nil {
			return
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

func runProgrammatic(ctx context.Context, h *dun.Harness, em *emitter, in *inputStream, firstTask string, cc closeCtx) {
	em.emit(event{"type": "ready", "tools": h.ToolNames(), "servers": serversToAny(h.Servers()),
		"hint": serverHint(h.Servers())})
	// Server commands (/rag, /lsp) are handled on the READER's goroutine, not
	// here: this loop is blocked inside a turn most of the time, and "turn the
	// docs server on" should not queue behind a five-minute agent run. The
	// harness defers the actual tool-set swap to a turn boundary.
	in.setServerCmd(func(alias, action string) {
		msg := runServerCmd(ctx, h, alias, action)
		em.emit(event{"type": "server", "id": alias, "action": action, "message": msg,
			"servers": serversToAny(h.Servers()), "tools": h.ToolNames()})
	})
	in.setCtrlCmd(func(id, action string) {
		// /close is handled here rather than in runControlCmd because it is
		// about the SESSION, not the harness: it needs the workspace root and
		// the session id, which only this scope has.
		if id == "close" {
			em.emit(event{"type": "control", "id": id, "action": action,
				"message": closeSession(cc.workspace, cc.sessionID, cc.wt)})
			return
		}
		msg := runControlCmd(ctx, h, id, action)
		em.emit(event{"type": "control", "id": id, "action": action, "message": msg})
	})
	in.setResetCb(func() {
		h.Reset()
		em.emit(event{"type": "reset"})
	})
	// Agent scope: the human reading a child's conversation, or steering it.
	// `view` is a PULL — the child's callbacks are nil by design, so its
	// scrollback is fetched on demand and works for a stopped child too.
	in.setAgentFn(func(id, action, content string) {
		n, err := strconv.Atoi(id)
		if err != nil {
			return
		}
		switch action {
		case "view":
			items, ok := h.AgentHistory(n)
			if !ok {
				return
			}
			em.emit(event{"type": "agent_history", "agent": n,
				"prompt": h.AgentPrompt(n), "items": items})
		case "tell":
			em.emit(event{"type": "agent_told", "agent": n, "message": h.AgentTell(n, content)})
		}
	})
	// A message that arrives while a turn is running does NOT wait for it. It is
	// buffered and lifted into the next tool result, so the model reads it inside
	// the turn it is already running — and several of them batch. Only when no turn
	// is in flight does a message start one (the channel path below).
	in.setMidTurn(func(text string) bool {
		if !turnActive.Load() {
			return false
		}
		h.Say(text)
		em.emit(event{"type": "queued", "text": text, "count": h.Queued()})
		return true
	})
	if firstTask != "" {
		turn(ctx, h, em, firstTask)
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
		// Anything buffered while the last turn ran (or after it failed on the
		// provider) is delivered now, in ONE turn — including a message the user
		// sent to nudge a dead session back to life.
		if autoContinue && h.Pending() > 0 {
			autoContinue = continueTurn(ctx, h, em)
			continue
		}
		if users == nil { // input is done; finish the background work, then leave
			if !drainStep(ctx, h, em) {
				em.emit(event{"type": "exit", "reason": "input closed"})
				return
			}
			continue
		}
		select {
		case content, ok := <-users:
			if !ok {
				if in.stopped() {
					em.emit(event{"type": "exit", "reason": "stopped"})
					return // explicit stop: don't wait for background jobs
				}
				// A closed channel is always ready in select, so stop selecting
				// on it and hand the loop to drainStep.
				users = nil
				continue
			}
			turn(ctx, h, em, content)
			autoContinue = true
		case <-h.Wake():
			// A background job finished; run a turn to process its notification.
			autoContinue = continueTurn(ctx, h, em)
		case <-ctx.Done():
			// Only ctrl-C reaches here now: --timeout bounds a turn, not the
			// session. Say so, rather than dying silently and leaving the TUI to
			// report a bare "engine exited".
			em.emit(event{"type": "exit", "reason": "interrupted"})
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
func drainStep(ctx context.Context, h *dun.Harness, em *emitter) bool {
	// A failed turn ends the drain rather than being retried. Unlike the
	// interactive loop there is nobody left to fix anything — stdin is closed —
	// so the pending work would fail identically forever.
	if h.BackgroundRunning() > 0 {
		select {
		case <-h.Wake():
		case <-ctx.Done():
			return false
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
func continueTurn(ctx context.Context, h *dun.Harness, em *emitter) bool {
	tctx, end := beginTurn(ctx)
	defer end()
	turnActive.Store(true)
	res, err := h.Continue(tctx)
	turnActive.Store(false)
	if err != nil {
		emitTurnError(ctx, em, err)
		return false
	}
	if strings.TrimSpace(res.Reply) != "" {
		em.emit(event{"type": "message", "role": "assistant", "content": res.Reply})
	}
	em.emit(event{"type": "usage", "total": res.Usage.Total, "active": res.Usage.Active,
		"cached": res.Usage.Cached, "processed": res.Usage.Processed,
		"generated": res.Usage.Generated, "turns": res.Usage.Turns})
	em.emit(event{"type": "done"})
	emitSuggestions(ctx, h, em)
	return true
}

// inputStream reads JSON events from stdin in a goroutine and routes them:
// user/stop → users, answer → answers. Decoupling the scanner from the turn loop
// lets an ask_user (blocked mid-turn) receive an answer.
//
// quit distinguishes the two ways users closes: an explicit stop/quit event
// closes it too, plain EOF does not. runProgrammatic drains background jobs on
// EOF but not on an explicit stop.
type inputStream struct {
	users   chan string
	answers chan string
	quit    chan struct{}

	// mid routes a user message that arrived while a turn was in flight. The turn
	// loop is not selecting on users then, so without this the scanner blocks —
	// and blocking it also stalls the `answer` events an ask_user needs. Guarded
	// because it is set after the harness exists, while the scanner already runs.
	mu  sync.Mutex
	mid func(string) bool
	// srv handles a `server` event (/rag, /lsp) inline, for the same reason as
	// mid: it must work while a turn is running.
	srv func(alias, action string)
	// srvPending holds server commands that arrived before the handler was
	// installed. The scanner starts before the harness exists, so a client that
	// writes its first line immediately would otherwise have it dropped.
	srvPending [][2]string
	// ctrl handles a `control` event (/docker, /worktree) inline.
	ctrl func(id, action string)
	// ctrlPending holds control commands that arrived before the handler was
	// installed.
	ctrlPending [][2]string
	// resetCb handles a `reset` event (clear the session store).
	resetCb func()
	// agentFn handles an `agent` event: the human viewing a child's transcript
	// or telling it something. Same install-after-the-harness-exists problem as
	// srv and ctrl, so it queues the same way.
	agentFn      func(id, action, content string)
	agentPending [][3]string
}

// setMidTurn installs the mid-turn router (see inputStream.mid).
func (s *inputStream) setMidTurn(f func(string) bool) {
	s.mu.Lock()
	s.mid = f
	s.mu.Unlock()
}

// setServerCmd installs the server-command handler (see inputStream.srv) and
// replays anything that arrived before it existed.
func (s *inputStream) setServerCmd(f func(alias, action string)) {
	s.mu.Lock()
	s.srv = f
	queued := s.srvPending
	s.srvPending = nil
	s.mu.Unlock()
	for _, c := range queued {
		f(c[0], c[1])
	}
}

// setCtrlCmd installs the control-command handler (see inputStream.ctrl) and
// replays anything that arrived before it existed.
func (s *inputStream) setCtrlCmd(f func(id, action string)) {
	s.mu.Lock()
	s.ctrl = f
	queued := s.ctrlPending
	s.ctrlPending = nil
	s.mu.Unlock()
	for _, c := range queued {
		f(c[0], c[1])
	}
}

// setResetCb installs the session-reset handler.
func (s *inputStream) setResetCb(f func()) {
	s.mu.Lock()
	s.resetCb = f
	s.mu.Unlock()
}

// agentCmd runs the installed agent handler, queueing until one exists.
func (s *inputStream) agentCmd(id, action, content string) {
	s.mu.Lock()
	fn := s.agentFn
	if fn == nil {
		s.agentPending = append(s.agentPending, [3]string{id, action, content})
	}
	s.mu.Unlock()
	if fn != nil {
		fn(id, action, content)
	}
}

// setAgentFn installs the handler and drains whatever arrived first.
func (s *inputStream) setAgentFn(fn func(id, action, content string)) {
	s.mu.Lock()
	s.agentFn = fn
	pending := s.agentPending
	s.agentPending = nil
	s.mu.Unlock()
	for _, p := range pending {
		fn(p[0], p[1], p[2])
	}
}

// serverCmd runs the installed handler, queueing until one exists.
func (s *inputStream) serverCmd(alias, action string) {
	s.mu.Lock()
	f := s.srv
	if f == nil {
		s.srvPending = append(s.srvPending, [2]string{alias, action})
	}
	s.mu.Unlock()
	if f != nil {
		f(alias, action)
	}
}

// ctrlCmd runs the installed control handler, queueing until one exists.
func (s *inputStream) ctrlCmd(id, action string) {
	s.mu.Lock()
	f := s.ctrl
	if f == nil {
		s.ctrlPending = append(s.ctrlPending, [2]string{id, action})
	}
	s.mu.Unlock()
	if f != nil {
		f(id, action)
	}
}

// midTurn offers text to the router, reporting whether it took it.
func (s *inputStream) midTurn(text string) bool {
	s.mu.Lock()
	f := s.mid
	s.mu.Unlock()
	return f != nil && f(text)
}

// stopped reports whether users closed because of an explicit stop/quit event
// rather than EOF. Safe to call only after users is closed: the scanner closes
// quit first, so the happens-before is via that close.
func (s *inputStream) stopped() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func newInputStream() *inputStream { return newInputStreamFrom(os.Stdin) }

// newInputStreamFrom is newInputStream over an arbitrary reader, so a test can
// drive the event parsing and the EOF-vs-stop distinction without a real stdin.
func newInputStreamFrom(r io.Reader) *inputStream {
	s := &inputStream{users: make(chan string), answers: make(chan string), quit: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Value   string `json:"value"`
				ID      string `json:"id"`     // server/control: which target
				Action  string `json:"action"` // server/control: action to take
			}
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "server":
				// Handled inline (see inputStream.srv): a turn may be running,
				// and this loop is also the only reader of `answer` events.
				s.serverCmd(ev.ID, ev.Action)
			case "control":
				// Handled inline (see inputStream.ctrl): docker/worktree toggles.
				s.ctrlCmd(ev.ID, ev.Action)
			case "agent":
				// The human looking at, or steering, a sub-agent. Inline for the
				// same reason as the others: a turn may be running, and this
				// loop is the only reader of `answer` events.
				s.agentCmd(ev.ID, ev.Action, ev.Content)
			case "user":
				if s.midTurn(ev.Content) {
					continue // buffered into the running turn
				}
				s.users <- ev.Content
			case "answer":
				s.answers <- ev.Value
			case "reset":
				s.mu.Lock()
				cb := s.resetCb
				s.mu.Unlock()
				if cb != nil {
					cb()
				}
			case "stop", "quit":
				close(s.quit) // before users, so stopped() is visible to the reader
				close(s.users)
				return
			}
		}
		close(s.users) // EOF: runProgrammatic drains background jobs before exiting
	}()
	return s
}

// humanAsk prompts on the terminal and reads a line. A number picks an option;
// with multi, comma-separated numbers (e.g. "1,3") pick several.
func humanAsk(_ context.Context, question string, options []string, multi bool) (string, error) {
	fmt.Fprintf(os.Stderr, "\n❓ %s\n", question)
	for i, o := range options {
		fmt.Fprintf(os.Stderr, "   %d) %s\n", i+1, o)
	}
	if multi && len(options) > 0 {
		fmt.Fprint(os.Stderr, "answer (comma-separated numbers for several): ")
	} else {
		fmt.Fprint(os.Stderr, "answer: ")
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if multi && len(options) > 0 && strings.ContainsAny(line, ", ") {
		var picked []string
		for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
			if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(options) {
				picked = append(picked, options[n-1])
			}
		}
		if len(picked) > 0 {
			return strings.Join(picked, ", "), nil
		}
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
		return options[n-1], nil
	}
	return line, nil
}

// suggestEnabled mirrors --suggest; turn/continueTurn emit next-message
// suggestions after `done` when set.
var suggestEnabled bool

// turnTimeout is --timeout applied PER TURN (interactive engine only; 0 = none).
// See the comment where it is set.
var turnTimeout time.Duration

// A turn that hangs is cut off without taking the session with it — the next
// message starts a fresh turn against a live context. See beginTurn.

// turnActive is true while a turn is in flight, so the input reader knows to
// BUFFER a user message (delivered inside the running turn, via the lift path)
// instead of handing it to the loop as the start of a new one.
var turnActive atomic.Bool

// turn runs one user message to completion. It reports whether the turn
// succeeded — NOT whether the engine should stop. A failed turn never ends the
// session: the conversation is on disk and the next message picks up from it.
func turn(ctx context.Context, h *dun.Harness, em *emitter, task string) bool {
	tctx, end := beginTurn(ctx)
	defer end()
	turnActive.Store(true)
	res, err := h.Ask(tctx, task)
	turnActive.Store(false)
	if err != nil {
		emitTurnError(ctx, em, err)
		return false
	}
	em.emit(event{"type": "message", "role": "assistant", "content": res.Reply})
	em.emit(event{"type": "usage", "total": res.Usage.Total, "active": res.Usage.Active,
		"cached": res.Usage.Cached, "processed": res.Usage.Processed,
		"generated": res.Usage.Generated, "turns": res.Usage.Turns})
	em.emit(event{"type": "done"})
	emitSuggestions(ctx, h, em)
	return true
}

// emitTurnError reports a failed turn, and says whether the SESSION is over.
//
// Those are different facts and conflating them is what made a 30-minute
// deadline look like a crash: fatal is true only when the session context
// itself is gone (ctrl-C), in which case no message can help. Otherwise the
// turn is what died — the conversation is on disk, and the next message pairs
// off whatever was interrupted and continues from there.
func emitTurnError(sessionCtx context.Context, em *emitter, err error) {
	em.emit(event{"type": "error", "error": err.Error(), "fatal": sessionCtx.Err() != nil})
}

// emitSuggestions asks for likely next user messages and emits them (best-effort,
// after `done` so the reply shows first). No-op unless --suggest.
func emitSuggestions(ctx context.Context, h *dun.Harness, em *emitter) {
	if !suggestEnabled {
		return
	}
	sugs, err := h.Suggestions(ctx)
	if err != nil || len(sugs) == 0 {
		return
	}
	items := make([]any, len(sugs))
	for i, s := range sugs {
		items[i] = map[string]any{"text": s.Text, "prob": s.Prob}
	}
	em.emit(event{"type": "suggestions", "items": items})
}

// retryEvent renders a RetryNote as a -p event. Durations travel as milliseconds
// (JSON has no duration type, and a client that wants to count down needs the
// number, not a formatted string) alongside the ready-made one-liner in "text".
func retryEvent(n dun.RetryNote) event {
	ev := event{"type": "retry", "scope": n.Scope, "kind": n.Kind,
		"attempt": n.Attempt, "status": n.Status,
		"delay_ms": n.Delay.Milliseconds(), "elapsed_ms": n.Elapsed.Milliseconds(),
		"budget_ms": n.Budget.Milliseconds(),
		"reason":    n.Reason, "detail": n.Detail, "text": n.String(),
		"server_asked": n.ServerAsked}
	// Queue numbers only when the provider actually reported them: a plain 429
	// carries none, and emitting zeros claims a queue of no slots.
	if n.Queued() {
		ev["capacity"], ev["in_flight"], ev["waiting"] = n.Capacity, n.InFlight, n.Waiting
	}
	if n.Queue != "" {
		ev["queue"] = n.Queue
	}
	return ev
}

// retryGlyph marks the three states a retry note can be in, for the human stream.
func retryGlyph(n dun.RetryNote) string {
	switch n.Kind {
	case "recovered":
		return "✓"
	case "giveup":
		return "✗"
	}
	return "⏳"
}

type event map[string]any

// emitter writes one JSON event per line to stdout, serialized (tokens stream
// from the same goroutine as turns today, but the mutex keeps it safe if that
// changes).
type emitter struct {
	mu  sync.Mutex
	w   io.Writer // nil → stdout; a test points this at a buffer
	enc *json.Encoder
}

func (e *emitter) emit(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		w := e.w
		if w == nil {
			w = os.Stdout
		}
		e.enc = json.NewEncoder(w)
	}
	_ = e.enc.Encode(ev)
}

func shortArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, clip(fmt.Sprint(args[k]), 40)))
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// docsToAny renders surfaced docs as JSON-friendly maps for the -p event.
func agentsToAny(as []dun.AgentInfo) []any {
	out := make([]any, 0, len(as))
	for _, a := range as {
		out = append(out, map[string]any{
			"id": a.ID, "model": a.Model, "state": a.State, "status": a.Status,
			"prompt": a.Prompt, "tokens": a.Tokens, "seconds": a.Seconds, "blocked": a.Blocked,
		})
	}
	return out
}

// jobsToAny mirrors agentsToAny — the whole list every time, for the same
// reason. Times cross as unix seconds so the UI can tick a running job's clock
// without the engine pushing a row per second.
func jobsToAny(js []dun.JobInfo) []any {
	out := make([]any, 0, len(js))
	for _, j := range js {
		out = append(out, map[string]any{
			"id": j.ID, "command": j.Command, "log": j.Log, "state": j.State,
			"bytes": j.Bytes, "started": j.Started, "ended": j.Ended,
			"code": j.Code, "muted": j.Muted,
		})
	}
	return out
}

func docsToAny(docs []dun.DocHitInfo) []any {
	out := make([]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, map[string]any{"title": d.Title, "id": d.DocID, "line": d.Line, "score": d.Score})
	}
	return out
}

// closeSession discards this session for good: its worktree, its branch, and
// its transcript, so /resume cannot offer it back.
//
// Deliberately destructive and deliberately explicit — it is the counterpart to
// the startup prune, which refuses to delete anything holding work. This is how
// a human says "that work is finished with", which is the only way that call
// can safely be made.
func closeSession(workspace, sessionID string, wt *dun.Worktree) string {
	var parts []string
	if wt != nil && wt.Branch != "" {
		root := dun.RepoRoot(workspace)
		if err := dun.RemoveWorktree(root, wt.Path, wt.Branch); err != nil {
			parts = append(parts, "worktree NOT removed: "+err.Error())
		} else {
			parts = append(parts, "removed worktree "+wt.Branch)
		}
	}
	if sessionID != "" {
		dun.ForgetSession(workspace, sessionID)
		parts = append(parts, "forgot session "+sessionID)
	}
	if len(parts) == 0 {
		return "nothing to close — this session has no worktree and no transcript"
	}
	return "closed: " + strings.Join(parts, "; ")
}

// reportHousekeeping says what was cleaned and, more importantly, what was not.
// An abandoned worktree is the one thing here a human has to decide about.
func reportHousekeeping(res dun.PruneResult, sessions []string) {
	if n := len(res.Pruned); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: pruned %s (%s)\n", plural(n, "stale worktree"), humanBytes(res.Freed))
	}
	if n := len(sessions); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: pruned %s\n", plural(n, "old session"))
	}
	if n := len(res.Abandoned); n > 0 {
		fmt.Fprintf(os.Stderr, "dun: %s still hold work —\n", plural(n, "abandoned worktree"))
		for _, w := range res.Abandoned {
			fmt.Fprintf(os.Stderr, "  %s\n", w.Summary())
		}
		fmt.Fprintf(os.Stderr, "  (resume one with --resume, or discard it from the TUI with /close)\n")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n/(1<<20))
	}
	return fmt.Sprintf("%d KB", n/(1<<10))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "dun: %v\n", err)
	os.Exit(1)
}
