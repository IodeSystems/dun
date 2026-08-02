package dun

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Tool servers start and stop while a session runs.
//
// Two of the three (code = poly-lsp-mcp, docs = raglit) are opt-in: they cost
// seconds of startup and a chunk of the tool budget, and a session that never
// touches them should not pay. That makes the tool set MUTABLE, which is the
// whole difficulty here — an agent.Session's Tools, Dispatch, System and
// Preparer are all derived from which servers are up, so every start/stop ends
// in applyTools recomputing the four together. Anything that sets one of them
// directly will be silently reverted by the next /rag or /lsp.
//
// Failure is not fatal, ever. A missing binary or a server that refuses to run
// is reported (ServerState.Err) and the session carries on without that
// family — losing an in-flight conversation because raglit is misconfigured
// would be a worse outcome than losing search.

// ServerState is one tool server's state, for a UI.
type ServerState struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	// Auto reports whether this server starts on its own next session.
	Auto  bool   `json:"auto"`
	Tools int    `json:"tools"`
	Err   string `json:"err,omitempty"` // why the last start attempt failed
}

// Servers reports every configured server and whether it is up, in config
// order. Servers a config file disabled outright are not listed — they are not
// something a session can turn on.
func (h *Harness) Servers() []ServerState {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	counts := map[string]int{}
	for _, t := range h.mgr.GetTools() {
		counts[t.ServerID]++
	}
	out := make([]ServerState, 0, len(h.specs))
	for _, s := range h.specs {
		out = append(out, ServerState{
			ID:      s.ID,
			Running: h.mgr.ServerStarted(s.ID),
			Auto:    s.Autostart,
			Tools:   counts[s.ID],
			Err:     h.lastErr[s.ID],
		})
	}
	return out
}

// StartServer spawns a configured server by id and rebuilds the tool set.
// Starting one that is already running is a no-op, not a restart: a restart
// would drop whatever state the server has built up (raglit's index, the LSP's
// analysis) for a command the user meant as "make sure this is on".
func (h *Harness) StartServer(ctx context.Context, id string) error {
	h.srvMu.Lock()
	spec, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return fmt.Errorf("dun: no server %q (have %s)", id, strings.Join(ServerIDs(h.specs), ", "))
	}
	if h.mgr.ServerStarted(id) {
		return nil
	}
	return h.startServer(ctx, spec)
}

// StopServer shuts a server down and rebuilds the tool set without it.
func (h *Harness) StopServer(id string) error {
	h.srvMu.Lock()
	_, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return fmt.Errorf("dun: no server %q", id)
	}
	if !h.mgr.ServerStarted(id) {
		return nil
	}
	h.mgr.StopServer(id)
	h.applyTools()
	return nil
}

// SetAutostart persists whether a server spawns at startup, and updates this
// session's view of it so Servers() reports the new setting immediately.
// Returns the config file written.
func (h *Harness) SetAutostart(id string, on bool) (string, error) {
	h.srvMu.Lock()
	_, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return "", fmt.Errorf("dun: no server %q", id)
	}
	dir := h.cfg.ConfigDir
	if dir == "" {
		dir = h.cfg.Workspace
	}
	path, err := SetAutostart(dir, id, on)
	if err != nil {
		return "", err
	}
	h.srvMu.Lock()
	for i := range h.specs {
		if h.specs[i].ID == id {
			h.specs[i].Autostart = on
		}
	}
	h.srvMu.Unlock()
	return path, nil
}

// spec finds a configured server by id. Caller holds srvMu.
func (h *Harness) spec(id string) (Server, bool) {
	for _, s := range h.specs {
		if s.ID == id {
			return s, true
		}
	}
	return Server{}, false
}

// startServer spawns one server, waits for its tools, and rebuilds the tool
// set. The error it returns is also recorded, so a UI can show why a family is
// missing long after the attempt.
func (h *Harness) startServer(ctx context.Context, s Server) error {
	timeout := time.Duration(s.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	// raglit answers nothing until the workspace is in its index, and the index
	// is a per-session temp dir — so ingest belongs to STARTING the docs server,
	// not to starting dun. A session that never turns rag on never pays for it.
	if s.ID == ServerDocs && h.cfg.RaglitHome != "" {
		ingestWorkspace(s.Command, h.cfg.RaglitHome, h.cfg.Workspace)
	}
	err := h.mgr.StartServer(ctx, mcpmgr.MCPConfig{
		ID: s.ID, Name: s.ID, Command: s.Command, Args: s.Args,
		Env: s.Env, Timeout: int(timeout / time.Second),
	})
	if err == nil {
		err = waitServerTools(ctx, h.mgr, s.ID, timeout)
		if err != nil {
			h.mgr.StopServer(s.ID) // half-started is worse than not started
		}
	}
	h.srvMu.Lock()
	if err != nil {
		h.lastErr[s.ID] = err.Error()
	} else {
		delete(h.lastErr, s.ID)
	}
	h.srvMu.Unlock()
	h.applyTools()
	return err
}

// waitServerTools blocks until the named server has reported at least one tool,
// its discovery has failed, or timeout. Discovery is async — StartServer
// returning nil only means the handshake worked.
func waitServerTools(ctx context.Context, mgr *mcpmgr.Manager, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := mgr.ServerReadyErr(id); err != nil {
			return err
		}
		for _, t := range mgr.GetTools() {
			if t.ServerID == id {
				return nil
			}
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("dun: %s: no tools discovered after %s", id, timeout)
			if tail := mgr.ServerStderr(id); tail != "" {
				msg += " — server stderr:\n" + tail
			}
			return fmt.Errorf("%s", msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// applyTools rebuilds the Session's tool set, deferring to the end of the
// current turn if one is running. Safe from any goroutine: server commands
// arrive on the -p reader while a turn may be mid-flight, and rewriting
// Session.Tools under a running turn is a data race.
func (h *Harness) applyTools() {
	h.applyPending.Store(true)
	if !h.turnMu.TryLock() {
		return // runTurn applies it on the way out
	}
	defer h.turnMu.Unlock()
	if h.applyPending.Swap(false) {
		h.rebuildTools()
	}
}

// rebuildTools recomputes everything derived from the running servers: the tool
// definitions the model sees, the dispatcher that routes calls, the system
// prompt's tool-family list, and the proactive-docs preparer. Callers hold
// turnMu (or are in Start, before any turn can run).
func (h *Harness) rebuildTools() {
	if h.Session == nil {
		return // autostart runs before the Session exists; Start applies it after
	}
	tools := h.mgr.GetTools()
	// Tell the model what changed under it, if anything. Buffered, not sent:
	// see Harness.Aside for why a tool-set change is never worth a turn of its
	// own. Skipped on the first build — the system prompt covers that.
	if h.toolsInit {
		if note := toolSetDelta(h.Tools, tools); note != "" {
			h.Aside(note)
		}
	}
	h.toolsInit = true
	h.Tools = tools

	// Bridge the MCP tools + the built-in tools (exec, ask_user). Non-MCP tools
	// are handled locally by the dispatcher wrappers; everything else routes to
	// its MCP server.
	cfg := h.cfg
	toolDefs := mcpToolDefs(tools)
	dispatch := mcpDispatcher(h.mgr, tools, cfg.OnToolCall)
	if cfg.Exec != nil {
		toolDefs = append(toolDefs, execToolDef(), execMonitorToolDef())
		startBg := func(command string) *bgJob { return h.startBackground(cfg.Exec, command) }
		dispatch = withExec(dispatch, cfg.Exec, cfg.OnToolCall, startBg)
		dispatch = withExecMonitor(dispatch, h, cfg.OnToolCall)
	}
	if cfg.Ask != nil {
		toolDefs = append(toolDefs, askToolDef())
		dispatch = withAsk(dispatch, cfg.Ask, cfg.OnToolCall)
	}
	// Sub-agent tools are chosen by ROLE, and that choice IS the enforcement:
	// a child never receives `agent`, so depth-1 holds with no counter to get
	// wrong, and a root never receives `tell_parent`, so it is absent rather
	// than being a tool that errors when called. See plan/subagents.md.
	if h.isChild() {
		toolDefs = append(toolDefs, tellParentToolDef(), askParentToolDef())
		dispatch = withTellParent(dispatch, h, cfg.OnToolCall)
		dispatch = withAskParent(dispatch, h, cfg.OnToolCall)
	} else {
		toolDefs = append(toolDefs, agentToolDef(cfg.ChildModel), agentMonitorToolDef())
		dispatch = withAgent(dispatch, h, cfg.OnToolCall)
		dispatch = withAgentMonitor(dispatch, h, cfg.OnToolCall)
	}
	// recap is available to BOTH roles, and the difference is the confirmation:
	// a root asks the human before rewriting a conversation they are part of,
	// a child has nobody to ask and its context is exactly what this is for.
	toolDefs = append(toolDefs, recapToolDef())
	dispatch = withRecap(dispatch, h, cfg.OnToolCall)
	// Watches what every OTHER tool call produces, so a suggestion arrives at
	// the moment churn is created rather than whenever usage is next measured.
	dispatch = withRecapWatch(dispatch, h)

	// Outermost wrapper: whatever the tool returns, carry any notification that
	// arrived while it was running back inside the result. A background job that
	// finishes mid-turn is then reported in the result the model is already
	// reading, instead of scheduling a turn of its own.
	dispatch = withLiftedQueue(dispatch, h)

	sys := cfg.System
	if sys == "" {
		sys = systemFor(tools, cfg.Exec, cfg.Worktree)
		sys += roleSystem(h.isChild())
	}
	// ship is the only way work leaves the worktree. It needs a repo, not a
	// branch: a --no-worktree session sits on the base branch, and the pipeline
	// (rebase + checks) is worth running there too — it just refuses to push.
	if cfg.EnableShip && cfg.Worktree != nil && cfg.Worktree.IsRepo() {
		toolDefs = append(toolDefs, shipToolDef(cfg.ShipCfg))
		execFn := func(ctx context.Context, command string) ExecResult {
			if cfg.Exec != nil {
				return cfg.Exec.Run(ctx, command, nil)
			}
			// No backend is a FAILED check, not a passing one: ship must never
			// report "verified" for commands it could not run.
			return ExecResult{Code: -1, Err: "no exec backend configured"}
		}
		dispatch = withShip(dispatch, cfg.Worktree, cfg.ShipCfg, execFn, cfg.OnToolCall)
		sys += "\n\nWhen the task is complete, commit everything (including new files) and call ship. It fetches origin, rebases onto " +
			cfg.Worktree.BaseBranch + ", runs the project's checks, and lands the result in mode " +
			string(cfg.ShipCfg.defaultMode()) + " unless you name another. Do not push by hand with exec — ship is what verifies before it pushes."
		if cfg.Worktree.Branch != "" {
			sys += "\n\nYou are working on branch " + cfg.Worktree.Branch + " off " + cfg.Worktree.BaseBranch + ". When you commit, use a descriptive subject line (e.g. 'fix(parser): handle nested quotes' or 'feat(tui): add /clear command') that stands on its own — the branch name is just a timestamp."
		}
	}

	h.Session.Tools = toolDefs
	h.Session.Dispatch = dispatch
	h.Session.System = sys

	// Proactive RAG: watch the conversation and inject relevant-doc pings before
	// each turn (raglit's search tool as an agent.DocFinder). Injected notices
	// surface via store.onNotify → OnNotify. Cleared when docs stops, or the
	// preparer would call a dead server before every turn.
	h.Session.Preparer = nil
	if finder := docsFinder(h.mgr, tools); finder != nil {
		// MinScore 0: raglit's search is BM25, whose scores aren't in a fixed
		// range (tiny for a small index) — but a MATCH only returns matching
		// rows, so any hit is a real lexical hit. MaxHits caps what's surfaced.
		// dun's aggregating preparer emits one found/surfaced summary per pass.
		h.Session.Preparer = docsPreparer(h.store, finder, agent.FinderOpts{MaxHits: 2}, cfg.OnDocs)
	}
}

// toolSetDelta describes what appeared and disappeared, grouped by server, or
// "" when nothing changed.
//
// Names, not schemas: the schemas themselves ride every request in the tools
// block, so repeating them here would pay for the same bytes twice. What the
// model cannot see there is that the set CHANGED — it reasoned two turns ago
// about not having search, and will keep acting on that until told otherwise.
func toolSetDelta(before, after []mcpmgr.MCPTool) string {
	was := toolsByServer(before)
	now := toolsByServer(after)
	var lines []string
	for _, id := range serverOrder(was, now) {
		added := missing(now[id], was[id])
		removed := missing(was[id], now[id])
		switch {
		case len(was[id]) == 0 && len(added) > 0:
			lines = append(lines, fmt.Sprintf("%s is now available: %s", id, strings.Join(added, ", ")))
		case len(now[id]) == 0 && len(removed) > 0:
			lines = append(lines, fmt.Sprintf("%s was turned off; no longer callable: %s", id, strings.Join(removed, ", ")))
		default:
			if len(added) > 0 {
				lines = append(lines, fmt.Sprintf("%s gained: %s", id, strings.Join(added, ", ")))
			}
			if len(removed) > 0 {
				lines = append(lines, fmt.Sprintf("%s lost: %s", id, strings.Join(removed, ", ")))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "your tools changed — " + strings.Join(lines, "; ") +
		". Use what is there now; do not call what is gone."
}

func toolsByServer(tools []mcpmgr.MCPTool) map[string][]string {
	out := map[string][]string{}
	for _, t := range tools {
		out[t.ServerID] = append(out[t.ServerID], t.Name)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// serverOrder is every server mentioned by either side, deterministically.
func serverOrder(a, b map[string][]string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range []map[string][]string{a, b} {
		for id := range m {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// missing returns the entries of want that have are absent from have.
func missing(want, have []string) []string {
	in := make(map[string]bool, len(have))
	for _, s := range have {
		in[s] = true
	}
	var out []string
	for _, s := range want {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// ingestWorkspace lexically indexes the workspace into raglit (best-effort).
// raglitCmd is the configured docs binary, so an overridden path is honored;
// a docs server pointed at something that is not raglit is left alone.
func ingestWorkspace(raglitCmd, raglitHome, workspace string) {
	if filepath.Base(raglitCmd) != "raglit" {
		return
	}
	// Ingest the workspace's entries, NOT the workspace — so dun's own state
	// directory is left out.
	//
	// .dun holds the per-session git worktrees, which are COPIES of the
	// workspace. Handing raglit the workspace root indexed every one of them:
	// measured in this repo, 16 worktrees, and every workspace-wide search came
	// back entirely stale duplicates with the live files nowhere in the results.
	// An agent's own isolation mechanism poisoning its own search index is a
	// spectacular own goal, and the fix is to never hand it the directory.
	targets := ingestTargets(workspace)
	if len(targets) == 0 {
		return
	}
	args := append([]string{"ingest", "--embedded", "--home", raglitHome, "--now"}, targets...)
	// --embedded for the same reason the serve command needs it: a per-session
	// home has no project name, and the shared daemon refuses those.
	cmd := detach(exec.Command(raglitCmd, args...))
	if out, err := cmd.CombinedOutput(); err != nil {
		// Best-effort: proactive RAG simply has less to ping without it. Worth
		// logging, since "search finds nothing" is otherwise unexplained.
		log.Printf("dun: raglit ingest failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// ingestTargets lists what raglit should index: the workspace's top-level
// entries minus the two directories that are never content — git's object
// store, and dun's own state (which contains copies of the workspace).
func ingestTargets(workspace string) []string {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return []string{workspace} // unreadable: fall back to the old behaviour
	}
	var out []string
	for _, e := range entries {
		switch e.Name() {
		case ".git", DunDir:
			continue
		}
		out = append(out, filepath.Join(workspace, e.Name()))
	}
	return out
}
