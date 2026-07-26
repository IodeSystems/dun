// Package dun is a coding-agent harness: it composes agentkit (the engine) with
// three MCP tool servers — poly-lsp-mcp (semantic code), mcpshell (sandboxed
// compute), and raglit (docs/RAG) — into one agent that works a task inside an
// isolated workspace.
//
// Slice 1 is the headless composition: spawn the servers, bridge their tools
// into an agent.Session, run the tool loop. The Bubble Tea TUI and the Docker +
// git-worktree isolation layer on top (see plan/plan.md).
package dun

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Server is one MCP tool server dun spawns.
type Server struct {
	ID      string
	Command string
	Args    []string
	// Env entries ("KEY=value") for the spawned server — a per-machine DSN or
	// endpoint that cannot be baked into a committed config.
	Env []string
	// Timeout in seconds for tool discovery; 0 uses dun's default.
	Timeout int
}

// DefaultServers points the three tool servers at a workspace directory (later a
// git worktree, and later still `docker exec` into a container).
func DefaultServers(workspace, raglitHome string) []Server {
	return []Server{
		{ID: "code", Command: "poly-lsp-mcp", Args: []string{"mcp", "--root", workspace}},
		{ID: "shell", Command: "mcpshell", Args: []string{"mcp", "--files-dir", workspace}},
		{ID: "docs", Command: "raglit", Args: []string{"serve", "--home", raglitHome}},
	}
}

// Config configures a dun harness.
type Config struct {
	Workspace  string
	RaglitHome string

	Servers []Server // nil → DefaultServers(Workspace, RaglitHome)
	// ConfigDir is where dun.json / dun.local.json are looked for. Empty →
	// Workspace. Separated because the workspace may be an isolated worktree
	// while the config lives with the developer's checkout.
	ConfigDir   string
	Client      agent.LLMRunner // the LLM (e.g. *llm.Client)
	System      string          // nil → defaultSystem
	Exec        ExecBackend     // nil → no exec tool; else adds the built-in exec tool
	Ask         AskFunc         // nil → no ask_user tool; else adds the human-in-the-loop tool
	Worktree    *Worktree       // the session worktree (for open_pr)
	EnablePR    bool            // add the open_pr tool (opt-in: pushing + PR is outward-facing)
	SessionFile string          // persist the conversation here (resumable); "" = in-memory only
	OnToken     func(string)
	OnToolCall  func(tool string, args map[string]any, result string)
	// OnNotify fires when a plain notification (KindNotification) is injected
	// into the conversation (e.g. a background job's completion).
	OnNotify func(text string)
	// OnDocs fires when the proactive-RAG preparer surfaces relevant documents —
	// one aggregated summary per pass (found/surfaced counts + surfaced docs).
	OnDocs func(DocsNote)
}

// Harness is a running dun: the MCP manager + an agent Session over its tools.
type Harness struct {
	mgr     *mcpmgr.Manager
	Session *agent.Session
	Tools   []mcpmgr.MCPTool
	store   *sessionStore
	wake    chan struct{} // signals a driver to run a Continue turn (bg job done)
	bgMu    sync.Mutex
	bgSeq   int
	bgRun   int // background jobs still running
	noteMu  sync.Mutex
	notes   []string // notifications not yet delivered to the model
}

// Resumed reports how many entries were restored from an existing session file.
func (h *Harness) Resumed() int { return h.store.Loaded() }

// Notify hands the model a proactive notification (a finished background job).
//
// The note is buffered rather than published straight into the inbox, because
// WHEN it reaches the model decides how much it costs:
//
//   - If a tool call is in flight, the note rides back inside that tool's
//     result (see liftNotes). The model is already waiting on that result, so
//     the news costs no extra turn and cannot land as a stray assistant message.
//   - Otherwise flushNotes publishes whatever is left when the turn ends, as
//     one inbox arrival per note, and wakes the driver ONCE for all of them.
//
// OnNotify still fires immediately so a UI shows the job finishing in real time.
func (h *Harness) Notify(text string) {
	h.noteMu.Lock()
	h.notes = append(h.notes, text)
	h.noteMu.Unlock()
	if cb := h.store.notifyCallback(); cb != nil {
		cb(text)
	}
}

// liftNotes drains buffered notifications into a tool result, so a job that
// finished mid-turn is reported inside the result the model is already reading.
// Returns result unchanged when nothing is buffered.
func (h *Harness) liftNotes(result string) string {
	h.noteMu.Lock()
	notes := h.notes
	h.notes = nil
	h.noteMu.Unlock()
	if len(notes) == 0 {
		return result
	}
	var b strings.Builder
	b.WriteString(result)
	for _, n := range notes {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("[background] ")
		b.WriteString(n)
	}
	return b.String()
}

// flushNotes publishes any notification the turn did not pick up, as real inbox
// arrivals. Returns how many were flushed so the caller can decide whether a
// follow-up turn is worth running.
func (h *Harness) flushNotes() int {
	h.noteMu.Lock()
	notes := h.notes
	h.notes = nil
	h.noteMu.Unlock()
	for _, n := range notes {
		h.store.publishNotificationSilent(agent.Entry{
			ID: uuid.New().String(), Kind: agent.KindNotification,
			Content: n, CreatedAt: time.Now().UnixNano(),
		})
	}
	return len(notes)
}

// Pending reports whether a Continue turn has anything new to process —
// unclaimed inbox arrivals or buffered notifications.
func (h *Harness) Pending() int {
	h.noteMu.Lock()
	buffered := len(h.notes)
	h.noteMu.Unlock()
	return h.store.pending() + buffered
}

// Wake fires when a background job finishes, so the driver runs a Continue turn.
func (h *Harness) Wake() <-chan struct{} { return h.wake }

// BackgroundRunning is how many background jobs are still in flight.
func (h *Harness) BackgroundRunning() int {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	return h.bgRun
}

// Continue runs a turn with NO new user message — to process pending
// notifications (e.g. a background job's completion). This is the converge
// point: the notification (+ any queued messages) coalesce into one turn.
func (h *Harness) Continue(ctx context.Context) (agent.TurnResult, error) {
	// Publish anything a tool result did not already carry, so the turn below
	// has it — and so several jobs finishing at once become ONE turn.
	h.flushNotes()
	if h.store.pending() == 0 {
		// Nothing new to react to. Running the model anyway appends an assistant
		// message directly after the previous one, which providers reject on the
		// following request. A duplicate wake is a no-op, not a turn.
		return agent.TurnResult{}, nil
	}
	return h.Session.Turn(ctx)
}

// startBackground runs command asynchronously via backend (a container when
// DockerExec); on completion it injects a completion notification and wakes the
// driver. Returns the job id.
func (h *Harness) startBackground(backend ExecBackend, command string) int {
	h.bgMu.Lock()
	h.bgSeq++
	id := h.bgSeq
	h.bgRun++
	h.bgMu.Unlock()
	go func() {
		out := strings.TrimSpace(backend.Run(context.Background(), command))
		h.bgMu.Lock()
		h.bgRun--
		h.bgMu.Unlock()
		h.Notify(fmt.Sprintf("background job #%d finished — `%s`:\n%s", id, command, out))
		select {
		case h.wake <- struct{}{}:
		default: // wake is buffered; a full buffer just means a turn is already due
		}
	}()
	return id
}

// Start spawns the servers, waits for tool discovery, and builds the Session.
func Start(ctx context.Context, cfg Config) (*Harness, error) {
	// Explicit Go-level Servers win outright — a caller that constructed them
	// meant it. Otherwise resolve the layered config files, which fall back to
	// the built-in trio when neither exists.
	servers := cfg.Servers
	if servers == nil {
		dir := cfg.ConfigDir
		if dir == "" {
			dir = cfg.Workspace
		}
		var err error
		servers, err = LoadServers(dir, cfg.Workspace, cfg.RaglitHome)
		if err != nil {
			return nil, err
		}
	}
	mgr := mcpmgr.NewManager()
	for _, s := range servers {
		timeout := s.Timeout
		if timeout == 0 {
			timeout = 90
		}
		if err := mgr.StartServer(ctx, mcpmgr.MCPConfig{
			ID: s.ID, Name: s.ID, Command: s.Command, Args: s.Args,
			Env: s.Env, Timeout: timeout,
		}); err != nil {
			mgr.Close()
			return nil, fmt.Errorf("dun: start %s: %w", s.ID, err)
		}
	}
	tools, err := waitForTools(ctx, mgr, len(servers))
	if err != nil {
		mgr.Close()
		return nil, err
	}

	sys := cfg.System
	if sys == "" {
		sys = defaultSystem
	}
	store, err := openSessionStore(cfg.SessionFile)
	if err != nil {
		mgr.Close()
		return nil, fmt.Errorf("dun: open session: %w", err)
	}
	store.onNotify = cfg.OnNotify
	h := &Harness{mgr: mgr, Tools: tools, store: store, wake: make(chan struct{}, 16)}

	// Bridge the MCP tools + the built-in tools (exec, ask_user). Non-MCP tools
	// are handled locally by the dispatcher wrappers; everything else routes to
	// its MCP server.
	toolDefs := mcpToolDefs(tools)
	dispatch := mcpDispatcher(mgr, tools, cfg.OnToolCall)
	if cfg.Exec != nil {
		toolDefs = append(toolDefs, execToolDef())
		startBg := func(command string) int { return h.startBackground(cfg.Exec, command) }
		dispatch = withExec(dispatch, cfg.Exec, cfg.OnToolCall, startBg)
	}
	if cfg.Ask != nil {
		toolDefs = append(toolDefs, askToolDef())
		dispatch = withAsk(dispatch, cfg.Ask, cfg.OnToolCall)
	}
	// Outermost wrapper: whatever the tool returns, carry any notification that
	// arrived while it was running back inside the result. A background job that
	// finishes mid-turn is then reported in the result the model is already
	// reading, instead of scheduling a turn of its own.
	dispatch = withLiftedNotes(dispatch, h)
	if cfg.EnablePR && cfg.Worktree != nil && cfg.Worktree.Branch != "" {
		toolDefs = append(toolDefs, prToolDef())
		dispatch = withPR(dispatch, cfg.Worktree, cfg.OnToolCall)
		sys += "\n\nWhen the task is complete and verified (build/tests pass), call open_pr with a concise title and a summary body to submit your work as a pull request."
	}
	h.Session = &agent.Session{
		SessionID:        "dun",
		System:           sys,
		Store:            store,
		Runner:           cfg.Client,
		Tools:            toolDefs,
		Dispatch:         dispatch,
		OnAssistantToken: cfg.OnToken,
		MaxTurns:         maxTurns(),
	}
	// Proactive RAG: watch the conversation and inject relevant-doc pings before
	// each turn (raglit's search tool as an agent.DocFinder). Injected notices
	// surface via store.onNotify → OnNotify.
	if finder := docsFinder(mgr, tools); finder != nil {
		// MinScore 0: raglit's search is BM25, whose scores aren't in a fixed
		// range (tiny for a small index) — but a MATCH only returns matching
		// rows, so any hit is a real lexical hit. MaxHits caps what's surfaced.
		// dun's aggregating preparer emits one found/surfaced summary per pass.
		h.Session.Preparer = docsPreparer(store, finder, agent.FinderOpts{MaxHits: 2}, cfg.OnDocs)
	}
	return h, nil
}

// Ask injects a user message and runs the tool loop to completion.
func (h *Harness) Ask(ctx context.Context, task string) (agent.TurnResult, error) {
	h.store.publish(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindUser, Content: task, CreatedAt: time.Now().UnixNano(),
	})
	return h.Session.Turn(ctx)
}

// Close shuts down the MCP servers.
func (h *Harness) Close() { h.mgr.Close() }

// ToolNames lists the agent's tool names (MCP tools + the built-in exec), sorted.
func (h *Harness) ToolNames() []string {
	names := make([]string, len(h.Session.Tools))
	for i, t := range h.Session.Tools {
		names[i] = t.Function.Name
	}
	sort.Strings(names)
	return names
}

// waitForTools polls until every spawned server has reported at least one tool
// (or a timeout). Discovery is async — GetTools returns nothing until a server
// finishes its MCP handshake.
func waitForTools(ctx context.Context, mgr *mcpmgr.Manager, wantServers int) ([]mcpmgr.MCPTool, error) {
	for i := 0; i < 120; i++ {
		tools := mgr.GetTools()
		seen := map[string]bool{}
		for _, t := range tools {
			seen[t.ServerID] = true
		}
		if len(seen) >= wantServers && len(tools) > 0 {
			return tools, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	// Return whatever we got; the caller can proceed with a partial tool set.
	tools := mgr.GetTools()
	if len(tools) == 0 {
		return nil, fmt.Errorf("dun: no MCP tools discovered after timeout (are poly-lsp-mcp/mcpshell/raglit on PATH?)")
	}
	return tools, nil
}

// defaultSystem is dun's coding-agent persona + tool guidance.
const defaultSystem = `You are dun, a coding agent working inside an isolated workspace.

You have three tool families:
- code (poly-lsp-mcp): node_query to find/navigate code by selector (call it with selector "?" to learn the grammar), node_read to read a symbol whole, node_edit to edit/rename/refactor. Edits return diagnostics.
- shell (mcpshell): eval runs sandboxed script code for computation, data wrangling, and jailed file ops; call the prompt tool for its language reference, help to list commands.
- docs (raglit): search the document/knowledge index; ingest to add sources.
- exec: run a shell command (build/test/git/ls) in the workspace. Use it to VERIFY your edits — e.g. run the build and tests after changing code — and to run git.
- ask_user: when the task is ambiguous or a decision is the user's to make (which approach, which file, is this OK to change), call ask_user with a clear question and optional options INSTEAD of guessing.

Relevant docs may be pushed to you as [docs] notes — use them.

Work step by step: find with node_query, read what you need, make minimal precise edits, verify via the diagnostics AND by running the build/tests with exec. Prefer node_edit over rewriting files. Be concise. When the task is done, briefly summarize what you changed.`

// maxTurns is the cap on agent loop iterations. 40 suits an interactive
// session, where the user is present and can nudge; a long autonomous task on a
// large repo can legitimately need far more, since paging through unfamiliar
// files costs turns before any edit happens. DUN_MAX_TURNS raises it.
func maxTurns() int {
	if v := os.Getenv("DUN_MAX_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 40
}

// withLiftedNotes appends any notification buffered during a tool call to that
// tool's result. The model is already reading the result, so a background job
// that finished mid-turn is reported with no extra turn — and, critically, no
// assistant message appended after another assistant message.
func withLiftedNotes(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		out, err := inner(ctx, tc)
		if err != nil {
			return out, err
		}
		return h.liftNotes(out), nil
	}
}
