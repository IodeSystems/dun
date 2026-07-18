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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Server is one MCP tool server dun spawns.
type Server struct {
	ID      string
	Command string
	Args    []string
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
	Servers    []Server        // nil → DefaultServers(Workspace, RaglitHome)
	Client     agent.LLMRunner // the LLM (e.g. *llm.Client)
	System     string          // nil → defaultSystem
	Exec       ExecBackend     // nil → no exec tool; else adds the built-in exec tool
	Ask        AskFunc         // nil → no ask_user tool; else adds the human-in-the-loop tool
	OnToken    func(string)
	OnToolCall func(tool string, args map[string]any, result string)
	// OnNotify fires when a proactive notification (KindNotification) is injected
	// into the conversation (e.g. a relevant-doc ping from the RAG finder).
	OnNotify func(text string)
}

// Harness is a running dun: the MCP manager + an agent Session over its tools.
type Harness struct {
	mgr     *mcpmgr.Manager
	Session *agent.Session
	Tools   []mcpmgr.MCPTool
	store   *memStore
	wake    chan struct{} // signals a driver to run a Continue turn (bg job done)
	bgMu    sync.Mutex
	bgSeq   int
	bgRun   int // background jobs still running
}

// Notify injects a proactive notification into the conversation inbox (claimed
// on the next turn) and fires OnNotify.
func (h *Harness) Notify(text string) {
	h.store.publishNotification(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindNotification, Content: text, CreatedAt: time.Now().UnixNano(),
	})
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
	servers := cfg.Servers
	if servers == nil {
		servers = DefaultServers(cfg.Workspace, cfg.RaglitHome)
	}
	mgr := mcpmgr.NewManager()
	for _, s := range servers {
		if err := mgr.StartServer(ctx, mcpmgr.MCPConfig{
			ID: s.ID, Name: s.ID, Command: s.Command, Args: s.Args, Timeout: 90,
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
	store := newMemStore()
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
	h.Session = &agent.Session{
		SessionID:        "dun",
		System:           sys,
		Store:            store,
		Runner:           cfg.Client,
		Tools:            toolDefs,
		Dispatch:         dispatch,
		OnAssistantToken: cfg.OnToken,
		MaxTurns:         40,
	}
	// Proactive RAG: watch the conversation and inject relevant-doc pings before
	// each turn (raglit's search tool as an agent.DocFinder). Injected notices
	// surface via store.onNotify → OnNotify.
	if finder := docsFinder(mgr, tools); finder != nil {
		// MinScore 0: raglit's search is BM25, whose scores aren't in a fixed
		// range (tiny for a small index) — but a MATCH only returns matching
		// rows, so any hit is a real lexical hit. MaxHits caps the noise.
		h.Session.Preparer = agent.FinderPreparer(store, finder, agent.FinderOpts{MaxHits: 2, Tag: "docs"})
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
