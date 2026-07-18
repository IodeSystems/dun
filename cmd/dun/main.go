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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

func main() {
	url := flag.String("url", "https://llm.iodesystems.com", "LLM base URL")
	model := flag.String("model", "ternary-bonsai-27b", "chat model (must support tool calls)")
	key := flag.String("key", os.Getenv("DUN_LLM_KEY"), "API key (or $DUN_LLM_KEY)")
	ws := flag.String("workspace", ".", "workspace directory (a git repo → worktree isolation)")
	docker := flag.String("docker", "", "run exec commands in a Docker container of this image (empty = host)")
	noWorktree := flag.Bool("no-worktree", false, "work in the workspace directly, no git worktree")
	pr := flag.Bool("pr", false, "let the agent open a pull request (commit+push+gh pr create) when done")
	cont := flag.Bool("continue", false, "resume the most recent session for this workspace")
	resume := flag.String("resume", "", "resume a specific session id (see --sessions)")
	listSessions := flag.Bool("sessions", false, "list saved sessions for this workspace and exit")
	prog := flag.Bool("p", false, "programmatic mode: emit + read line-delimited JSON events")
	tui := flag.Bool("tui", false, "launch the interactive Bubble Tea UI")
	serve := flag.Bool("serve", false, "serve a web UI (a browser client of -p) at --addr")
	addr := flag.String("addr", "127.0.0.1:8734", "serve: HTTP listen address")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout")
	flag.Parse()
	firstTask := strings.TrimSpace(strings.Join(flag.Args(), " "))

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

	// TUI mode: a Bubble Tea client of `dun -p` (re-exec'd with the same flags).
	if *tui {
		if err := runTUI(tuiOpts{absWS, *model, *url, *key, *docker, *noWorktree, *pr, *cont, *resume}); err != nil {
			fatal(err)
		}
		return
	}

	// Serve mode: a web client of `dun -p` (same re-exec, bridged over SSE/POST).
	if *serve {
		if err := runServe(tuiOpts{absWS, *model, *url, *key, *docker, *noWorktree, *pr, *cont, *resume}, *addr); err != nil {
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
	if firstTask == "" && !*prog {
		fmt.Fprintln(os.Stderr, `usage: dun [--workspace DIR] "task"   (or -tui, or -p for JSON events)`)
		os.Exit(2)
	}
	raglitHome, err := os.MkdirTemp("", "dun-raglit-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(raglitHome)

	// Isolation tier 1: a git worktree (unless --no-worktree). The agent's file
	// changes land here on a fresh branch, not on the checked-out branch.
	effWS := absWS
	var wt *dun.Worktree
	if !*noWorktree {
		w, isRepo, werr := dun.NewWorktree(absWS)
		if werr != nil {
			fatal(werr)
		}
		wt, effWS = w, w.Path
		if !isRepo && !*prog {
			fmt.Fprintf(os.Stderr, "dun: %s is not a git repo — working in place (no isolation)\n", absWS)
		}
	}

	// Isolation tier 2: exec runs in a Docker container (--docker IMAGE), or host.
	var backend dun.ExecBackend
	if *docker != "" {
		backend = dun.DockerExec{Dir: effWS, Image: *docker}
	} else {
		backend = dun.HostExec{Dir: effWS}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// Best-effort: index the workspace into raglit (lexical, fast) so proactive
	// doc-notifications + search have content.
	ingestWorkspace(raglitHome, effWS)

	var em *emitter
	var in *inputStream
	cfg := dun.Config{
		Workspace:  effWS,
		RaglitHome: raglitHome,
		Client:     llm.NewClient(*url, *key, *model),
		Exec:        backend,
		Worktree:    wt,
		EnablePR:    *pr,
		SessionFile: sessionFile,
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
		cfg.OnDocs = func(n dun.DocsNote) {
			em.emit(event{"type": "notification", "kind": "docs", "found": n.Found, "surfaced": n.Surfaced, "docs": docsToAny(n.Docs)})
		}
		cfg.Ask = func(actx context.Context, q string, opts []string, multi bool) (string, error) {
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
		cfg.Ask = humanAsk
		fmt.Fprintf(os.Stderr, "dun: spawning tool servers for %s …\n", absWS)
	}

	h, err := dun.Start(ctx, cfg)
	if err != nil {
		if em != nil {
			em.emit(event{"type": "error", "error": err.Error()})
		}
		fatal(err)
	}
	defer h.Close()

	if *prog {
		em.emit(event{"type": "session", "id": sessionID, "resumed": h.Resumed()})
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
		runProgrammatic(ctx, h, em, in, firstTask)
		return
	}
	runHuman(ctx, h, firstTask)

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
func runHuman(ctx context.Context, h *dun.Harness, task string) {
	fmt.Fprintf(os.Stderr, "dun: %d tools ready: %s\n\ntask: %s\n\n",
		len(h.ToolNames()), strings.Join(h.ToolNames(), ", "), task)
	res, err := h.Ask(ctx, task)
	if err != nil {
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
func runProgrammatic(ctx context.Context, h *dun.Harness, em *emitter, in *inputStream, firstTask string) {
	em.emit(event{"type": "ready", "tools": h.ToolNames()})
	if firstTask != "" {
		turn(ctx, h, em, firstTask)
	}
	for {
		select {
		case content, ok := <-in.users:
			if !ok {
				return // stdin closed / stop
			}
			turn(ctx, h, em, content)
		case <-h.Wake():
			// A background job finished; run a turn to process its notification.
			continueTurn(ctx, h, em)
		case <-ctx.Done():
			return
		}
	}
}

// continueTurn runs a turn with no new user message (to process a background
// job's completion notification) and emits its events.
func continueTurn(ctx context.Context, h *dun.Harness, em *emitter) {
	res, err := h.Continue(ctx)
	if err != nil {
		em.emit(event{"type": "error", "error": err.Error()})
		return
	}
	if strings.TrimSpace(res.Reply) != "" {
		em.emit(event{"type": "message", "role": "assistant", "content": res.Reply})
	}
	em.emit(event{"type": "usage", "total": res.Usage.Total, "active": res.Usage.Active})
	em.emit(event{"type": "done"})
}

// inputStream reads JSON events from stdin in a goroutine and routes them:
// user/stop → users, answer → answers. Decoupling the scanner from the turn loop
// lets an ask_user (blocked mid-turn) receive an answer.
type inputStream struct {
	users   chan string
	answers chan string
}

func newInputStream() *inputStream {
	s := &inputStream{users: make(chan string), answers: make(chan string)}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
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
			}
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "user":
				s.users <- ev.Content
			case "answer":
				s.answers <- ev.Value
			case "stop", "quit":
				close(s.users)
				return
			}
		}
		close(s.users)
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

// ingestWorkspace lexically indexes the workspace into raglit (best-effort).
func ingestWorkspace(raglitHome, workspace string) {
	cmd := exec.Command("raglit", "ingest", "--home", raglitHome, "--now", workspace)
	_ = cmd.Run() // best-effort; proactive RAG simply has less to ping without it
}

func turn(ctx context.Context, h *dun.Harness, em *emitter, task string) {
	res, err := h.Ask(ctx, task)
	if err != nil {
		em.emit(event{"type": "error", "error": err.Error()})
		return
	}
	em.emit(event{"type": "message", "role": "assistant", "content": res.Reply})
	em.emit(event{"type": "usage", "total": res.Usage.Total, "active": res.Usage.Active})
	em.emit(event{"type": "done"})
}

type event map[string]any

// emitter writes one JSON event per line to stdout, serialized (tokens stream
// from the same goroutine as turns today, but the mutex keeps it safe if that
// changes).
type emitter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (e *emitter) emit(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		e.enc = json.NewEncoder(os.Stdout)
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
func docsToAny(docs []dun.DocHitInfo) []any {
	out := make([]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, map[string]any{"title": d.Title, "id": d.DocID, "line": d.Line, "score": d.Score})
	}
	return out
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
