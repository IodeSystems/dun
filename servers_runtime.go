package dun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
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
// directly will be silently reverted by the next /raglit or /poly-lsp.
//
// Failure is not fatal, ever. A missing binary or a server that refuses to run
// is reported (ServerState.Err) and the session carries on without that
// family — losing an in-flight conversation because raglit is misconfigured
// would be a worse outcome than losing search.

// filterTools applies per-server deny-lists. If a server's Disable field
// is non-empty, the tools it names are dropped; all others pass through.
// Mirrors Claude's --disable-tools: forward-compatible, new tools pass
// through by default. An entry is a tool name or a glob ("tts_*", path.Match).
//
// The deny-list is resolved per tool across the scopes (Server.toolState).
func filterTools(tools []mcpmgr.MCPTool, specs []Server) []mcpmgr.MCPTool {
	by := make(map[string]Server, len(specs))
	for _, s := range specs {
		by[s.ID] = s
	}
	out := make([]mcpmgr.MCPTool, 0, len(tools))
	for _, t := range tools {
		if on, _, _ := by[t.ServerID].toolState(t.Name); on {
			out = append(out, t)
		}
	}
	return out
}

// hiddenBy is the first Disable entry that hides name, or "". A malformed
// glob still matches its own exact text.
func hiddenBy(name string, disable []string) string {
	for _, p := range disable {
		if ok, err := path.Match(p, name); ok || (err != nil && p == name) {
			return p
		}
	}
	return ""
}

// ServerState is one tool server's state, for a UI.
type ServerState struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	// Auto reports whether this server starts on its own next session.
	Auto  bool   `json:"auto"`
	Tools int    `json:"tools"`
	Err   string `json:"err,omitempty"` // why the last start attempt failed
	// Missing: Err is a MissingError — the command is not installed, a setup
	// step rather than a failure (missing.go).
	Missing bool `json:"missing,omitempty"`
	// Starting is a start in flight: launched, not yet answering with its
	// tools (Running may already be true: the handshake is done, discovery
	// is not). StartingSeconds is how long it has been at it.
	Starting        bool    `json:"starting,omitempty"`
	StartingSeconds float64 `json:"startingSeconds,omitempty"`
	StartSeconds    float64 `json:"startSeconds,omitempty"` // how long the last start took, in seconds
	// Via is an added server's placement: ViaDirect or ViaShell; "" for a
	// built-in, which has no choice.
	Via string `json:"via,omitempty"`
	// For a shell-placed server: Hosted is true when the running shell
	// connected it (Tools is then its tool count, Err why it did not), and
	// Namespace is what the model calls it by.
	Hosted    bool   `json:"hosted,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Cost is what the enabled tools' schemas cost in every request (see
	// toolTokens); Hidden is how many discovered tools Disable hides.
	Cost   int `json:"cost,omitempty"`
	Hidden int `json:"hidden,omitempty"`
	// Global, Project and Session are the on/off switch at each scope
	// ("on", "off", "" unset; Global is never unset); From is the scope that
	// decided Auto. ViaGlobal, ViaProject and ViaSession are the placement at
	// each scope ("direct", "shell", ""), ViaFrom the scope that decided Via.
	// Proto is how dun reaches it: "stdio" (a command) or the URL's scheme.
	Proto      string `json:"proto,omitempty"`
	Global     string `json:"global,omitempty"`
	Project    string `json:"project,omitempty"`
	Session    string `json:"session,omitempty"`
	From       string `json:"from,omitempty"`
	ViaGlobal  string `json:"viaGlobal,omitempty"`
	ViaProject string `json:"viaProject,omitempty"`
	ViaSession string `json:"viaSession,omitempty"`
	ViaFrom    string `json:"viaFrom,omitempty"`
	// Auth is a remote server's credentials: its OAuth login state, "static
	// bearer", "static header" or "none"; "" for a local server.
	Auth string `json:"auth,omitempty"`
	// Ingest is the last workspace ingest's summary, on the server it went
	// to (a workspace.open extension): what was indexed and what was skipped
	// (IngestPlan.Summary). "" for every other server.
	Ingest string `json:"ingest,omitempty"`
	// Slash is the extension's slash command ("raglit"), "" for a server
	// that is not an extension or has none.
	Slash string `json:"slash,omitempty"`
	// Index is the index a server builds after its handshake
	// (indexstate.go): IndexBuilding, IndexReady, IndexFailed, or "" for a
	// server that builds none or says nothing of it (an older poly-lsp-mcp).
	// IndexSeconds is how long it has been building, or how long the build
	// took; IndexErr why it failed; IndexFiles the files it counted.
	// Exclude is the directories left out of it, for this project.
	Index        string   `json:"index,omitempty"`
	IndexSeconds float64  `json:"indexSeconds,omitempty"`
	IndexErr     string   `json:"indexErr,omitempty"`
	IndexFiles   int      `json:"indexFiles,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	IndexBudget  string   `json:"indexBudget,omitempty"` // the project's --index-budget, "" the default
}

// Servers reports every configured server and whether it is up, in config
// order. Servers a config file disabled outright are not listed — they are not
// something a session can turn on.
func (h *Harness) Servers() []ServerState {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	counts, cost, hidden := map[string]int{}, map[string]int{}, map[string]int{}
	by := map[string]Server{}
	for _, s := range h.specs {
		by[s.ID] = s
	}
	for _, t := range h.mgr.GetTools() {
		if on, _, _ := by[t.ServerID].toolState(t.Name); !on {
			hidden[t.ServerID]++
			continue
		}
		counts[t.ServerID]++
		cost[t.ServerID] += h.toolTokens(t)
	}
	shellUp := h.mgr.ServerStarted(ServerShell)
	out := make([]ServerState, 0, len(h.specs))
	for _, s := range h.specs {
		st := ServerState{
			ID:           s.ID,
			Running:      h.mgr.ServerStarted(s.ID),
			Auto:         s.Autostart,
			Tools:        counts[s.ID],
			Err:          h.lastErr[s.ID],
			Missing:      h.missing[s.ID],
			StartSeconds: h.lastStart[s.ID].Seconds(),
			Cost:         cost[s.ID],
			Hidden:       hidden[s.ID],
			Auth:         h.authState(s),
			Proto:        serverProto(s),
			Global:       onOffWord(s.Scopes.GlobalOn(s.Via)),
			Project:      tri(s.Scopes.Project),
			Session:      tri(s.Scopes.Session),
			From:         s.Scopes.OnFrom,
			ViaGlobal:    s.Scopes.ViaGlobal,
			ViaProject:   s.Scopes.ViaProject,
			ViaSession:   s.Scopes.ViaSession,
			ViaFrom:      s.Scopes.ViaFrom,
		}
		if t, ok := h.starting[s.ID]; ok {
			st.Starting, st.StartingSeconds = true, time.Since(t).Seconds()
		}
		if s.ID == h.ingestID {
			st.Ingest = h.ingestNote
		}
		if e, ok := h.extension(s.ID); ok {
			st.Slash = e.SlashName()
		}
		if st.Running {
			h.indexOf(s.ID, &st)
		}
		st.Exclude, st.IndexBudget = s.Exclude, s.IndexBudget
		switch {
		case s.Via == ViaShell:
			st.Via, st.Namespace, st.Err, st.Tools = ViaShell, ShellNamespace(s.ID), "", 0
			if hs, ok := h.hosted[s.ID]; ok && shellUp {
				st.Hosted, st.Tools, st.Err = hs.Connected, hs.Tools, hs.Err
			}
		case !builtinServer(s.ID):
			st.Via = ViaDirect
		}
		out = append(out, st)
	}
	return out
}

// serverProto is "stdio" for a command, else the URL's scheme.
func serverProto(s Server) string {
	if s.URL == "" {
		return "stdio"
	}
	if scheme, _, ok := strings.Cut(s.URL, "://"); ok {
		return strings.ToLower(scheme)
	}
	return "http"
}

// tri is a switch as shown: "on", "off", or "" unset.
func tri(b *bool) string {
	if b == nil {
		return ""
	}
	return onOffWord(*b)
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
	if h.mgr.ServerStarted(id) || h.isStarting(id) {
		return nil
	}
	return h.startServer(ctx, spec)
}

// serverStartGrace is how long a session waits for its autostart servers
// before it is ready without the ones still starting. Long enough for the
// shell and a small repo's servers (well under a second each), short enough
// that the person never waits on a slow one. A var for tests.
var serverStartGrace = 1500 * time.Millisecond

// waitGrace waits for wg, or for d, whichever is first. The goroutines it
// stops waiting for keep running.
func waitGrace(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// markStarting records that a start of id is in flight, from now unless one
// already was.
func (h *Harness) markStarting(id string) {
	h.srvMu.Lock()
	if h.starting == nil {
		h.starting = map[string]time.Time{}
	}
	if _, ok := h.starting[id]; !ok {
		h.starting[id] = time.Now()
	}
	h.srvMu.Unlock()
}

// WaitServers waits until no server start is in flight, or ctx ends. For a
// one-shot run, whose single task should have every tool it was started
// with; an interactive session never waits.
func (h *Harness) WaitServers(ctx context.Context) {
	for {
		h.srvMu.Lock()
		n := len(h.starting)
		h.srvMu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// isStarting reports whether a start of id is in flight.
func (h *Harness) isStarting(id string) bool {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	_, ok := h.starting[id]
	return ok
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
	h.markIndexBuilding(id, false)
	h.applyTools()
	return nil
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
	if s.Via == ViaShell {
		return fmt.Errorf("%s is scripted — the model calls it through eval as %s; /mcp mode %s standalone to run it on its own",
			s.ID, ShellNamespace(s.ID), s.ID)
	}
	t0 := time.Now()
	h.markStarting(s.ID)
	timeout := time.Duration(s.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ext, isExt := h.extension(s.ID)
	// raglit answers nothing until the workspace is in its index, so ingest
	// belongs to STARTING the server (hook workspace.open), not to starting
	// dun: a session that never turns rag on never pays for it.
	//
	// The guard used to be `RaglitHome != ""`, which silently became "never"
	// the moment the per-session temp home was removed — the ingest was still
	// called, from a branch that could no longer be taken.
	if isExt && ext.Subscribes(HookWorkspaceOpen) {
		dir := h.cfg.ConfigDir
		if dir == "" {
			dir = h.cfg.Workspace
		}
		note := ingestWorkspace(h.indexSink(ext), h.cfg.Workspace, LoadRag(dir))
		h.srvMu.Lock()
		h.ingestNote, h.ingestID = note, s.ID
		h.srvMu.Unlock()
	}
	env := slices.Clone(s.Env) // appended to below; never the spec's own slice
	// Host hooks: the extension calls back into dun. poly-lsp's arms the
	// AGENTS.md guard, a file-access hook that cancels reads/edits until the
	// model has seen the project's rules.
	if isExt {
		env = append(env, hostHookEnv(ext, h.cfg.Workspace)...)
	}
	args := s.Args
	if s.ID == ServerShell {
		args = h.shellArgs(s) // + --mcp <file> when servers are placed in it
		env = h.shellEnv(s, env)
	}
	// A local command is looked for on PATH, then beside dun (missing.go);
	// one in neither place is not started at all.
	command := s.Command
	var err error
	if s.URL == "" && command != "" {
		command, err = FindCommand(command)
	}
	// A poly-lsp-mcp that builds its index after the handshake is
	// "indexing" from its spawn until it says how the build went, and takes
	// the project's excludes. An older one would refuse --exclude: it gets
	// none, and no index state.
	indexes, budgets := false, false
	if s.URL == "" && err == nil {
		indexes, budgets = h.speaksIndex(s, command)
	}
	if indexes {
		args = append(slices.Clone(args), excludeArgs(s.Exclude)...)
		// The project's startup budget, to a build that has one.
		if budgets && s.IndexBudget != "" {
			args = append(args, "--index-budget", s.IndexBudget)
		}
	}
	h.markIndexBuilding(s.ID, indexes)
	mcfg := mcpmgr.MCPConfig{
		ID: s.ID, Name: s.ID, Command: command, Args: args,
		Env: env, Timeout: int(timeout / time.Second),
		URL: s.URL, Headers: s.Headers,
	}
	// An OAuth server logs in BEFORE it connects: a login takes as long as
	// the person does, and must not run inside the handshake's timeout.
	if oc := h.oauthFor(s); err == nil && oc != nil {
		err = oc.ensure(ctx)
		mcfg.Authorization, mcfg.OnUnauthorized = oc.Authorization, oc.OnUnauthorized
	}
	if err == nil {
		err = h.mgr.StartServer(ctx, mcfg)
		if err != nil {
			err = explainStartErr(ctx, s, err)
		}
	}
	if err == nil {
		err = waitServerTools(ctx, h.mgr, s.ID, timeout)
		if err != nil {
			h.mgr.StopServer(s.ID) // half-started is worse than not started
		}
	}
	// Skills and prompts are read once, at start, so the tool rebuild below
	// can list them. Only for an added server: the built-ins serve neither.
	if err == nil && !builtinServer(s.ID) {
		h.discoverCatalog(ctx, s.ID)
	}
	if err == nil && s.ID == ServerShell {
		h.readHosted()
	}
	h.srvMu.Lock()
	if h.missing == nil {
		h.missing = map[string]bool{}
	}
	var miss *MissingError
	h.missing[s.ID] = errors.As(err, &miss)
	if err != nil {
		h.lastErr[s.ID] = err.Error()
	} else {
		delete(h.lastErr, s.ID)
		h.lastStart[s.ID] = time.Since(t0)
	}
	delete(h.starting, s.ID)
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
		// A server that serves only resources or prompts has no tools to wait
		// for: discovery finishing is the whole signal.
		if mgr.ServerReady(id) {
			return nil
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
	// Again while one is pending: a rebuild asked for while another rebuild
	// held the lock is this caller's to run, not the next turn's — a server
	// that comes up while the session is idle joins now.
	for h.applyPending.Load() {
		if !h.turnMu.TryLock() {
			return // a turn, or another rebuild, holds it; it applies on the way out
		}
		if h.applyPending.Swap(false) {
			h.rebuildTools()
		}
		h.turnMu.Unlock()
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
	tools := filterTools(h.mgr.GetTools(), h.specs)
	hostedNS := h.hostedNamespaces()
	moved := map[string]string{} // id → namespace, for servers now in the shell
	if len(hostedNS) > 0 {
		h.srvMu.Lock()
		for _, s := range h.hostedSpecs() {
			moved[s.ID] = ShellNamespace(s.ID)
		}
		h.srvMu.Unlock()
	}
	// Tell the model what changed under it, if anything. Buffered, not sent:
	// see Harness.Aside for why a tool-set change is never worth a turn of its
	// own. Skipped on the first build — the system prompt covers that.
	if h.toolsInit {
		if note := toolSetDelta(h.Tools, tools, moved); note != "" {
			h.Aside(note)
		}
	}
	h.toolsInit = true
	h.Tools = tools

	// Bridge the MCP tools + the built-in tools (exec, ask_user). Non-MCP tools
	// are handled locally by the dispatcher wrappers; everything else routes to
	// its MCP server.
	cfg := h.cfg
	// Every wrapper reports through this, so the -p stream and the TUI carry the
	// CLIPPED result — the one the model actually read.
	report := h.cappedReporter(cfg.OnToolCall)
	toolDefs := mcpToolDefs(tools)
	dispatch := mcpDispatcher(h.mgr, tools, report)
	// The extensions the hooks go to, among the running servers.
	exts := h.extensions()
	searchers := searchExts(exts, tools)
	// Attached indexes (attach.go): the search and get calls that name no
	// index read them too, and the search tool says which they are.
	attached := h.attachSelectors(searchers, tools)
	h.srvMu.Lock()
	describeAttached(toolDefs, searchers, h.specs, attached)
	h.srvMu.Unlock()
	dispatch = withAttached(dispatch, attachedTools(searchers, tools, attached))
	if cfg.Exec != nil {
		toolDefs = append(toolDefs, execToolDef(), execMonitorToolDef())
		// ONE backend for everything. A long command used to need a stateless
		// backend of its own so it would not hold the shell that every other
		// command serializes on; now it donates that shell when it is promoted
		// (HostShell.RunPromotable), so the persistent environment is available
		// to every command without any of them being able to block the rest.
		startJob := func(command string) *bgJob {
			return h.startJob(cfg.Exec, command)
		}
		dispatch = withExec(dispatch, cfg.Exec, report, startJob, h.spillExec, h.userWaiting)
		dispatch = withExecMonitor(dispatch, h, report)
	}
	// File tools, rooted at the working tree. Rebuilt with the tool set, so a
	// worktree switch (Rehoist) moves their root with exec's.
	ft := h.fileTools()
	if ft != nil {
		toolDefs = append(toolDefs, ft.Defs()...)
		dispatch = withFiles(dispatch, ft, h.tmpReaders(), report)
	}
	if cfg.Ask != nil {
		toolDefs = append(toolDefs, askToolDef())
		dispatch = withAsk(dispatch, cfg.Ask, report)
	}
	// Mount tool: only useful when exec runs inside Docker — there is no
	// container to mount into when exec is on the host.
	if cfg.Mount != nil && cfg.Exec != nil {
		_, isDocker := cfg.Exec.(DockerExec)
		if isDocker {
			toolDefs = append(toolDefs, mountToolDef())
			dispatch = withMount(dispatch, func(ctx context.Context, source, name string) (bool, error) {
				// Ask the user first, then apply the mount if approved.
				ok, err := cfg.Mount(ctx, source, name)
				if err != nil || !ok {
					return ok, err
				}
				// User approved — add the mount to the running session.
				if err := h.AddMount(source, name); err != nil {
					return false, err
				}
				return true, nil
			}, report)
		}
	}
	// dun_list and dun_msg: a root under dund only (cfg.Peers is nil
	// otherwise, and always for a child — see childConfig).
	if cfg.Peers != nil && !h.isChild() {
		toolDefs = append(toolDefs, dunListToolDef(), dunMsgToolDef())
		dispatch = withPeers(dispatch, cfg.Peers, report)
	}
	// Sub-agent tools are chosen by ROLE, and that choice IS the enforcement:
	// a child never receives `agent`, so depth-1 holds with no counter to get
	// wrong, and a root never receives `tell_parent`, so it is absent rather
	// than being a tool that errors when called. See plan/subagents.md.
	if h.isChild() {
		toolDefs = append(toolDefs, tellParentToolDef(), askParentToolDef())
		dispatch = withTellParent(dispatch, h, report)
		dispatch = withAskParent(dispatch, h, report)
	} else {
		toolDefs = append(toolDefs, agentToolDef(cfg.ChildModel), agentMonitorToolDef())
		dispatch = withAgent(dispatch, h, report)
		dispatch = withAgentMonitor(dispatch, h, report)
	}
	// recap is available to BOTH roles, and behaves identically in both: it
	// applies and then reports. It used to ask a root's human first — see the
	// rules at the top of recap.go for why that prompt was removed.
	toolDefs = append(toolDefs, recapToolDef())
	dispatch = withRecap(dispatch, h, report)
	// session_state is available to BOTH roles and, like recap, always on: it
	// is how a long task keeps its goal, plan, and open steps across context
	// pressure and process restarts. The state it manages is per-session, so a
	// child shares its parent's session file and thus its state.
	toolDefs = append(toolDefs, sessionStateToolDef())
	dispatch = withSessionState(dispatch, h, report)
	// memory only while a memory.save extension runs AND a turn.before one
	// does: a note is worth writing only because the proactive search brings
	// it back (see memory.go).
	h.memory.Store(nil)
	if e, ok := hookExt(exts, HookMemorySave, tools); ok && len(searchers) > 0 {
		m := memoryFor(h.indexSink(e), h.cfg.Workspace)
		h.memory.Store(m)
		toolDefs = append(toolDefs, memoryToolDef())
		dispatch = withMemory(dispatch, h, m, report)
	}
	// Skills into the index, off the turn lock: raglit takes a while.
	if e, ok := hookExt(exts, HookSkillsChanged, tools); ok {
		go h.syncSkillIndex(h.indexSink(e))
	}
	// skill, when any skill is in reach: the list rides the tool's
	// description, the body comes back from the tool.
	if list := skillList(h.Skills()); list != "" {
		toolDefs = append(toolDefs, skillToolDef(list))
		dispatch = withSkill(dispatch, h, report)
	}
	// Watches what every OTHER tool call produces, so a suggestion arrives at
	// the moment churn is created rather than whenever usage is next measured.
	dispatch = withRecapWatch(dispatch, h)

	// Outermost wrapper: whatever the tool returns, carry any notification that
	// arrived while it was running back inside the result. A background job that
	// finishes mid-turn is then reported in the result the model is already
	// reading, instead of scheduling a turn of its own.
	dispatch = withLiftedQueue(dispatch, h)

	sys := cfg.System
	rules, rulesMode, rulesSet := "", "", rulesSet{}
	if sys == "" {
		sys = systemFor(tools, cfg.Exec, cfg.Worktree, systemFiles(ft), hostedNS...)
		sys += roleSystem(h.isChild())
		// The workspace's root AGENTS.md and CLAUDE.md, and what they import,
		// are standing context: the model needs the project's rules from the
		// first message, and a system block survives compaction so a fold
		// never drops them (rules.go).
		_, rulesIndexed := hookExt(exts, HookRulesChanged, tools)
		rules, rulesMode, rulesSet = h.rulesBlock(rulesIndexed)
		sys += rules
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
		dispatch = withShip(dispatch, cfg.Worktree, cfg.ShipCfg, execFn, report)
		sys += "\n\nWhen the task is complete, commit everything (including new files) and call ship. It fetches origin, rebases onto " +
			cfg.Worktree.BaseBranch + ", runs the project's checks, and lands the result in mode " +
			string(cfg.ShipCfg.defaultMode()) + " unless you name another. Do not push by hand with exec — ship is what verifies before it pushes."
		if cfg.Worktree.Branch != "" {
			sys += "\n\nYou are working on branch " + cfg.Worktree.Branch + " off " + cfg.Worktree.BaseBranch + ". When you commit, use a descriptive subject line (e.g. 'fix(parser): handle nested quotes' or 'feat(tui): add /clear command') that stands on its own — the branch name is just a timestamp."
		}
	}

	// The session's durable state, when there is any, goes into the system
	// prompt so a resumed or compacted session reads its own goal and open
	// steps before it does anything else. Empty state renders nothing.
	if block := h.sessionStateBlock(); block != "" {
		sys += block
	}

	// Outermost of all, so it sees every tool including ship and recap — recap
	// being the one the measurement caught looping twelve deep. It must wrap the
	// wrappers rather than be wrapped by them: a call it refuses never runs, and
	// a refusal is the LAST thing that should be reconsidered.
	dispatch = withLoopGuard(dispatch, h)
	// Outside everything: a call the turn reaches after an interrupt is not
	// run at all (interrupt.go).
	dispatch = withInterrupt(dispatch)

	h.Session.Tools = toolDefs
	names := make([]string, len(toolDefs))
	for i, t := range toolDefs {
		names[i] = t.Function.Name
	}
	sort.Strings(names)
	h.toolNames.Store(&names)
	h.Session.Dispatch = dispatch
	h.Session.System = sys
	h.Session.OnToolCalls = h.mergeForcedToolCalls

	// Break the pre-conversation context into parts. Estimates land immediately
	// so /context is never empty; the exact counts replace them when they
	// arrive. See context_tokens.go for why this is not one number, why
	// exactness is all-or-nothing, and why the measurement cannot block here.
	files := rulesSet.files(rulesMode)
	est := measureSystem(context.Background(), nil, sys, rules, rulesMode, toolDefs, tools)
	est.Files = files
	h.setSystemBreakdown(est)
	go func() {
		bd := measureSystem(context.Background(), h.client, sys, rules, rulesMode, toolDefs, tools)
		bd.Files = files
		h.setSystemBreakdown(bd)
	}()
	// The rules index follows the mode: filled in index mode, emptied of an
	// earlier session's files otherwise. A root session's job, off the lock.
	if e, ok := hookExt(exts, HookRulesChanged, tools); ok && !h.isChild() {
		go h.syncRulesIndex(h.indexSink(e), rulesSet, rulesMode)
	}

	// Proactive RAG (hook turn.before): watch the conversation and inject
	// relevant-doc pings before each turn (the extensions' search role as an
	// agent.DocFinder). Injected notices surface via store.onNotify →
	// OnNotify. Cleared when the server stops, or the preparer would call a
	// dead server before every turn.
	// The budget notice (budget.go) rides the same seam, with or without a
	// docs server, so the preparer is never nil.
	var docs agent.NotificationPreparer
	if sf := newSurfacer(h.mgr, searchers, attached); sf != nil {
		// MinScore 0: raglit's score is BM25 or a rank fusion (hybrid), in
		// no fixed range. What surfaces is decided by the hit's similarity
		// or query-word coverage instead (see docsPreparer): one document
		// per group and pass, inlined when short.
		group := sf.group(searchers[0].ID, docGroup(h.memoryStore(), h.skillForDoc, h.isRulesDoc))
		home, _ := os.UserHomeDir()
		src := &hitSources{index: sf.idx.get, attached: sf.attached,
			display: func(p string) string { return displayPath(p, home, h.cfg.Workspace, h.cfg.ConfigDir) },
			meta:    sf.idx.metaOf, meaning: sf.meaning}
		docs = docsPreparer(h.store, sf.finder, agent.FinderOpts{}, h.surfacing, h.meter.Estimate,
			sf.fetch, group, h.skillForDoc, src, cfg.OnDocs)
	}
	h.docsPrep = docs
	h.Session.Preparer = h.withBudgetNotice(docs)
}

// toolSetDelta describes what appeared and disappeared, grouped by server, or
// "" when nothing changed.
//
// Names, not schemas: the schemas themselves ride every request in the tools
// block, so repeating them here would pay for the same bytes twice. What the
// model cannot see there is that the set CHANGED — it reasoned two turns ago
// about not having search, and will keep acting on that until told otherwise.
//
// moved maps a server now hosted in the shell to its namespace: its tools
// leaving the list is a move, not "turned off".
func toolSetDelta(before, after []mcpmgr.MCPTool, moved map[string]string) string {
	was := toolsByServer(before)
	now := toolsByServer(after)
	var lines []string
	for _, id := range serverOrder(was, now) {
		added := missing(now[id], was[id])
		removed := missing(was[id], now[id])
		ns, isMoved := moved[id]
		switch {
		case isMoved && len(now[id]) == 0 && len(removed) > 0:
			lines = append(lines, fmt.Sprintf("%s moved into the shell — call it through eval as %s.<tool>(…); help(%q) lists its commands; no longer callable directly: %s",
				id, ns, ns, strings.Join(removed, ", ")))
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
		out[t.ServerID] = append(out[t.ServerID], modelToolName(t)) // the name the model calls
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

// ingestWorkspace lexically indexes the workspace through sink (best-effort)
// and returns the summary the user sees (IngestPlan.Summary), "" when nothing
// was ingested. The index is raglitIndex(workspace), in the store the serve
// args name (raglit's --project) — the same as the serve command's, or the
// agent searches an index nothing was ingested into. A nil sink (a server
// that is not raglit, with no index tools) is left alone.
func ingestWorkspace(sink *mcpDocsSink, workspace string, cfg *RagConfig) string {
	if sink == nil {
		return ""
	}
	plan := planIngest(workspace, cfg)
	if len(plan.Files) == 0 {
		return ""
	}
	summary := plan.Summary()
	log.Printf("dun: %s", summary)
	// The sink chunks, so a large repository cannot overflow argv. Each call
	// queues and the daemon drains them.
	if err := sink.ingest(raglitIndex(workspace), plan.Files); err != nil {
		// Best-effort: proactive RAG simply has less to ping without it.
		// Worth logging, since "search finds nothing" is otherwise
		// unexplained.
		log.Printf("dun: workspace ingest failed: %v", err)
		return summary + " · ingest failed: " + err.Error()
	}
	return summary
}

// ingestBatch bounds one ingest's argv. Far under ARG_MAX anywhere dun runs; a
// repo bigger than this simply queues more than once.
const ingestBatch = 500

func chunkArgs(all []string, n int) [][]string {
	var out [][]string
	for len(all) > n {
		out, all = append(out, all[:n]), all[n:]
	}
	if len(all) > 0 {
		out = append(out, all)
	}
	return out
}

// ingestTargets lists what raglit should index under the default rules;
// planIngest (rag_ingest.go) has them.
//
// It used to hand over the workspace's top-level entries, which meant handing
// over build output. Measured in this repo: a 27 MB `dun` executable and a 25 MB
// `dun.test`, both gitignored, both sent to a pipeline that extracts, segments
// and embeds. Inside git the list is `git ls-files --cached --others
// --exclude-standard` (tracked files plus untracked ones .gitignore does not
// exclude); outside git, a walk that honors .gitignore. Both are then filtered
// by the junk, size and binary rules.
func ingestTargets(workspace string) []string {
	return planIngest(workspace, nil).Files
}

// builtinServer reports whether id is one of dun's own three servers: the
// shell and the bundled extensions.
func builtinServer(id string) bool {
	if canonicalServerID(id) == ServerShell {
		return true
	}
	_, ok := bundledExt(id)
	return ok
}

// serverIDPattern is what /mcp add accepts as an id: it becomes the tool
// namespace ("<id>__<tool>") and a raglit index name, so it is kept to
// characters both allow.
var serverIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// AddServer saves spec to the user server list ($DUN_HOME/mcp.json, every
// workspace) with autostart on, and starts it — restarting it when one of
// that id is running. It returns the file written; a server that saved but
// did not start returns both the path and the error.
func (h *Harness) AddServer(ctx context.Context, spec ServerSpec) (string, error) {
	if !serverIDPattern.MatchString(spec.ID) {
		return "", fmt.Errorf("an id is lowercase letters, digits, - and _ (got %q)", spec.ID)
	}
	if builtinServer(spec.ID) {
		return "", fmt.Errorf("%q is a built-in server; pick another id", spec.ID)
	}
	if (spec.URL == "") == (spec.Command == "") {
		return "", fmt.Errorf("a server has a url or a command, not both")
	}
	via, err := NormalizeVia(spec.Via)
	if err != nil {
		return "", err
	}
	spec.Via = via
	if err := checkOAuth(spec, via); err != nil {
		return "", err
	}
	on := true
	spec.Autostart = &on
	path, err := PutUserServer(spec)
	if err != nil {
		return "", err
	}
	// Same id replaces: bounce it (or the shell hosting it) for the new
	// definition. The other scopes' switches for it still apply.
	h.srvMu.Lock()
	old, had := h.spec(spec.ID)
	h.srvMu.Unlock()
	restart := map[string]bool{spec.ID: true}
	if had && old.Via == ViaShell {
		restart[ServerShell] = true
	}
	if err := h.reloadServers(ctx, true, restart); err != nil {
		return path, err
	}
	if s, err := h.knownServer(spec.ID); err == nil && s.Autostart && s.Via != ViaShell && !h.mgr.ServerStarted(s.ID) {
		return path, h.startServer(ctx, s)
	}
	return path, nil
}

// RemoveServer stops a server /mcp add saved and deletes it from the user
// server list. A server some other file declares is not this command's to
// remove. Its entries in the current project's state and in this session's
// sidecar go too, and are returned; other projects' files are not read.
func (h *Harness) RemoveServer(id string) ([]string, error) {
	ok, err := RemoveUserServer(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no server %q in %s — a server from dun.json is removed there", id, UserServersFile())
	}
	// Its stored login goes with it: a token for a server dun no longer
	// knows is a secret with no owner.
	_ = os.Remove(filepath.Join(OAuthDir(), id+".json"))
	h.oauthMu.Lock()
	delete(h.oauth, id)
	h.oauthMu.Unlock()
	cleared, err := removeLocalServer(h.configDir(), id)
	if err != nil {
		return cleared, fmt.Errorf("removed, but its project entry stays: %w", err)
	}
	h.srvMu.Lock()
	_, inSession := h.sessSw[id]
	h.srvMu.Unlock()
	if inSession {
		path, err := h.editSession(id, func(s *SessionServer) { *s = SessionServer{} })
		if err != nil {
			return cleared, fmt.Errorf("removed, but its session entry stays: %w", err)
		}
		if path != "" {
			cleared = append(cleared, path)
		}
	}
	if err := h.reloadServers(context.Background(), true, nil); err != nil {
		return cleared, fmt.Errorf("removed, but %w", err)
	}
	if h.mgr.ServerStarted(id) { // a project file still defines it; removed is removed
		h.mgr.StopServer(id)
		h.applyTools()
	}
	return cleared, nil
}
