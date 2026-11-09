// Package dun is a coding-agent harness: it composes agentkit (the engine) with
// three MCP tool servers — poly-lsp-mcp (semantic code), mcpshell (sandboxed
// compute), and raglit (docs/RAG) — into one agent that works a task inside an
// isolated workspace.
//
// Slice 1 is the headless composition: spawn the servers, bridge their tools
// into an agent.Session, run the tool loop. The Bubble Tea TUI and the Docker +
// git-worktree isolation layer on top (see docs/design.md).
package dun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mounttool"
	"github.com/iodesystems/dun/internal/procstat"
	"github.com/iodesystems/dun/internal/spendmeter"
	"github.com/iodesystems/dun/internal/tokenmeter"
)

// Config configures a dun harness.
type Config struct {
	Workspace string
	// RaglitHome overrides where raglit keeps its index. Empty — the normal
	// case — lets raglit resolve its own home and namespace this workspace by
	// project, so the index survives the session. It used to be a temp dir
	// deleted on exit, which is why nothing indexed ever outlived a run.
	RaglitHome string

	// Servers are what the config files layer over: nil → DefaultServers(
	// Workspace, RaglitHome). A child (Parent set) takes them as given.
	Servers []Server
	// ConfigDir is where dun.json / dun.local.json are looked for. Empty →
	// Workspace. Separated because the workspace may be an isolated worktree
	// while the config lives with the developer's checkout.
	ConfigDir string
	// AutostartOverride forces a server's autostart for THIS run only
	// (dun --rag / --lsp), above both the built-in default and the config
	// files. Nothing is persisted — that is what /rag auto is for.
	AutostartOverride map[string]bool
	Client            agent.LLMRunner // the LLM (e.g. *llm.Client)
	// ClientFor builds a runner for a NAMED model, so a sub-agent can run on a
	// cheaper one than its parent, on this session's provider ("") or another
	// logged-in one (USER 2026-10-30: run the Claude API and manage smaller
	// models). A factory rather than a client, because the dun package does
	// not know the endpoints or the keys — cmd/dun does. nil, or an error, on
	// the session's own provider means the child falls back to Client; on
	// another provider the spawn fails, since it would be billed elsewhere.
	ClientFor func(provider, model string) (ChildClient, error)
	// Providers are the logged-in providers a spawn may name, for the agent
	// tool's description.
	Providers []string
	// Spend totals what the gateway reports about money and limits (see
	// spend.go). The CALLER points each llm.Client's OnUsage at Spend.Note —
	// Client and every client ClientFor builds — since only it constructs them.
	// nil → no spend accounting.
	Spend *spendmeter.Meter
	// StatsLedger is where the root session's round stats go (roundstats.go);
	// nil writes none. cmd/dun points it at $DUN_HOME/stats.
	StatsLedger *StatsLedger
	// ChildModel is the default model for spawned sub-agents. Empty = the
	// parent's. A spawn may override it.
	ChildModel string
	// ChildProvider is the provider ChildModel is on; empty = this session's.
	ChildProvider string
	// ChildEffort is the default effort for spawned sub-agents; empty = the
	// saved /effort level. A spawn may override it.
	ChildEffort string
	// Effort is this harness's own effort level, in place of the saved one;
	// a child spawned with an effort has it. Empty = the saved level.
	Effort string
	// Files configures the built-in file tools (files.go); nil = the defaults
	// (DefaultFileTools: read, edit, write), rooted at Workspace.
	Files *FilesConfig
	// Parent, when set, makes this harness a CHILD of that one: it shares the
	// parent's MCP manager (same worktree root ⇒ same servers, so no second
	// index), reports to it, and gets the child tool set instead of the root's.
	Parent *Harness
	// AgentID is this child's id within its parent. Zero for a root.
	AgentID  int
	System   string         // nil → defaultSystem
	Exec     ExecBackend    // nil → no exec tool; else adds the built-in exec tool
	Ask      AskFunc        // nil → no ask_user tool; else adds the human-in-the-loop tool
	Mount    mounttool.Func // nil → no mount tool; else adds the mount (host path → container) tool
	Worktree *Worktree      // the session worktree
	// CommitCfg is how /worktree commit writes its message. nil = the
	// conventional-commits default.
	CommitCfg   *CommitConfig
	SessionFile string // persist the conversation here (resumable); "" = in-memory only
	// DockerImage is the image used when /isolate docker is chosen mid-session.
	// Set from the --docker flag at construction so the runtime toggle has
	// a default. Empty → "golang:1.23".
	DockerImage string
	// DockerNetwork controls whether the container has network access.
	// true (default) → default Docker bridge network (allows package download,
	// toolchain fetch, etc.). false → --network none (no egress).
	DockerNetwork bool
	// ExtraMounts are the mount specs passed at construction, needed when
	// reconfiguring DockerExec mid-session for a worktree switch.
	ExtraMounts []MountSpec
	OnToken     func(string)
	// OnReasoning fires with each delta of the model's reasoning, as it
	// streams, for the turn's own rounds only (see reasoning.go). nil → the
	// stream is not teed at all.
	OnReasoning func(string)
	// OnRequest fires with each turn round's speed and size figures
	// (reqstats.go): at its start, about 4 times a second while it streams,
	// at its end, and when a recap or compaction moves the context. The
	// turn's own rounds only. nil → not measured.
	OnRequest  func(RequestStat)
	OnToolCall func(tool string, args map[string]any, result string)
	// OnServerStarted reports an added (non-built-in) server that started in
	// the background at session start: err is nil when it is up and its
	// tools have joined the tool set.
	OnServerStarted func(id string, err error)
	// OnServerIndex reports a server's index build ending (indexstate.go):
	// msg is what to tell the person — what dun left out and how to undo
	// it, the post-mortem, a nested checkout — or "" when there is nothing
	// to say beyond the new state in Servers().
	OnServerIndex func(id, msg string)
	// OnPlan says the task plan changed (plan.go), whoever changed it: a
	// screen reads it again with Plan().
	OnPlan func()
	// OnServerSetup says a server's install hint (setupstate.go): one
	// quiet line, at most once per session.
	OnServerSetup func(id, msg string)
	// OnServerActivity says id's activity log has a new line (serverlog.go):
	// a step of a start, a line of its stderr. Often, during a start; a UI
	// coalesces.
	OnServerActivity func(id string)
	// Endpoint is the model in use and its address and key: the context
	// window set for a model is filed under it, and an OpenAI-compatible
	// model list is asked with it (window.go).
	Endpoint Endpoint
	// SystemNote is appended to the system prompt: what this run's caller
	// expects (dun -p "task": an answer after a delimiter, answer.go).
	SystemNote string
	// NoDir is a session attached to no directory (dun --no-dir, nodir.go):
	// no file tools, no rules, and no server that needs a project directory.
	NoDir bool
	// NonInteractive (--non-interactive): a run no one answers or reads
	// along, such as a script or a benchmark. The system prompt says the
	// model is on its own and leaves out what only a client uses (the
	// thinking headings, the Markdown note); cmd/dun gives it no Ask, so
	// there is no ask_user. {OUTPUT} stays: the engine expands it.
	NonInteractive bool
	// TurnLimit caps tool-call rounds per turn (--tool-call-turn-limit);
	// 0 is DUN_MAX_TURNS, else 400 (turnlimit.go).
	TurnLimit int
	// AsideEscalate is how long a silent job's or child's heartbeat waits
	// for a turn to carry it before it wakes the agent (--aside-escalate-ms;
	// 0 is DUN_ASIDE_ESCALATE_MS, else 5s; asidettl.go).
	AsideEscalate time.Duration
	// SettleDelay is how long the person may be away after a turn that used
	// tools before dun makes the agent report (--settle-delay; 0 is
	// DUN_SETTLE_DELAY, else 2m; negative is off; settle.go).
	SettleDelay time.Duration
	// PlanNudgeRounds and PlanNudgeFails are when the main agent is made to
	// write the plan: after this many rounds in a turn, or this many failed
	// tool results in a row, with no open plan node (--plan-nudge-rounds,
	// --plan-nudge-fails; 0 is DUN_PLAN_NUDGE_ROUNDS / DUN_PLAN_NUDGE_FAILS,
	// else 25 and 4; negative is off; plannudge.go).
	PlanNudgeRounds, PlanNudgeFails int
	// PlanStallRounds is when the main agent is made to update a plan that has
	// open nodes but no node moved in this many rounds, at most 3 times a turn
	// (--plan-stall-rounds; 0 is DUN_PLAN_STALL_ROUNDS, else 30; negative is
	// off; plannudge.go).
	PlanStallRounds int
	// NoTools is a plain chat (dun --no-tools): no tool of any kind reaches
	// the model, no server starts, and a call to a tool is refused.
	NoTools bool
	// Version and Provider are what the harness tool and dun session info
	// say (harnessinfo.go): the build and the provider's name.
	Version, Provider string
	// OnLogin shows the person the URL that logs in to an OAuth server
	// (mcpoauth.go); url "" means another session's login is being waited
	// for. The engine may have no display, so this is the way that always
	// works; OpenBrowser, when set, also tries to open it here.
	OnLogin     func(server, url string)
	OpenBrowser func(url string) error
	// OAuthBroker, when set, is dund: it holds every OAuth server's token,
	// refreshes it and runs the login (oauthbroker.go); OnLogin and
	// OpenBrowser are then dund's to do.
	OAuthBroker func(ctx context.Context, req OAuthRequest) (OAuthResult, error)
	// Gateway, when set, is dund's MCP gateway (gateway.go): a shared
	// server is reached through it, one connection for every session.
	Gateway MCPGateway
	// OnNotify fires when a plain notification (KindNotification) is injected
	// into the conversation (e.g. a background job's completion).
	OnNotify func(text string)
	// OnDocs fires when the proactive-RAG preparer surfaces relevant documents —
	// one aggregated summary per pass (found/surfaced counts + surfaced docs).
	OnDocs func(DocsNote)
	// OnMemoryTrigger fires when a turn's words ran the memory fork
	// (memtrigger.go), with the line for the user.
	OnMemoryTrigger func(msg string)
	// DisableMemoryTriggers stops that fork from running on its own;
	// /remember and the memory tool still work.
	DisableMemoryTriggers bool
	// OnCompaction fires when the Shaper folds history into a summary. Wired
	// because compaction is otherwise INVISIBLE: it is the one operation that
	// destroys conversation, and a session that was compacting on every turn
	// (see shapingBudget) ran for 29 minutes, wrote 154k characters of summary,
	// and left 5 entries alive without a single line of output saying so.
	OnCompaction func(CompactionNote)
	// OnAgents fires whenever a sub-agent changes — spawned, status, settled,
	// dismissed. The human's only view of what children are costing, since
	// jobs is model-facing and there is no concurrency cap.
	OnAgents func([]AgentInfo)
	// OnAgentEntry fires for each entry a WATCHED sub-agent's transcript gets
	// (Harness.AgentWatch): a person is reading that child's conversation.
	OnAgentEntry func(id int, items []HistoryItem)
	// Persist: the session persists (dund's /persist). Its sub-agents carry
	// the setting: a resume restarts the ones that were running or idle,
	// idle, instead of leaving them stopped (subagent.go, restoreAgents).
	Persist bool
	// OnJobs fires when a background job starts, finishes, or is re-tuned. Same
	// argument as OnAgents: jobs is model-facing, so without this a job
	// the human can see running has no place to be seen from.
	OnJobs func([]JobInfo)
	// OnSystemBreakdown fires when the pre-conversation cost is (re)measured:
	// once with estimates the moment tools are built, and again with exact
	// counts when the tokenizer answers.
	//
	// It exists because /context used to learn this number ONLY from the usage
	// event, which is emitted after a completed turn — so a session that had
	// done nothing but tool calls reported "not reported" for a figure the
	// harness had computed at startup. The cost of the context is knowable
	// before the first turn, and that is exactly when somebody asks.
	OnSystemBreakdown func(SystemBreakdown)
	// OnRecap fires when the model rewrites a span of its own history. The UI
	// gets a count and a citation, never the removed content — re-rendering the
	// churn a recap just took out would defeat it.
	OnRecap func(RecapNote)
	// OnRetry fires while dun is waiting on the provider — every backoff, the
	// recovery, and the give-up, at both request and turn scope. Without it the
	// wait is invisible: the retries are logged, and a TUI's logs are not on
	// screen. See RetryNote.
	OnRetry func(RetryNote)
	// Peers reaches the other live dun sessions through dund (peers.go). nil
	// — no dund on the other end of the engine's stdio — leaves dun_list and
	// dun_msg out of the tool set.
	Peers PeerLink
	// Loops reaches dund's scheduler, for loop_ctl (loopctl.go). nil with
	// Peers: no dund, no loops.
	Loops LoopLink
	// Rag reaches dund's retrieval, which holds the memory notes' indexes
	// (raglink.go, memrecall.go). Set, the session has the memory tool and
	// recalls notes, with dund down too (from the files); nil, neither.
	Rag RagLink
	// NoCodeSearch turns off the code and docs search through dund
	// (codesearch.go): DUN_CODE_SEARCH=0, as under the Harbor adapter.
	NoCodeSearch bool
	// Vision is what images on a user message need: which model Client runs
	// (for the vision probe record) and the vision→text model that describes
	// images for one that cannot read them. See images.go.
	Vision VisionConfig
	// OnRules tells the user something about the project's rules that is not
	// an answer to a command: the offer to make a large rules block cheaper
	// (rules.go).
	OnRules func(msg string)
}

// CompactionPause, if the host sets it, is called before the Shaper's
// compaction LLM call and returns the resume func. The host wires it to its
// turn budget clock so the fold STOPs the clock while it runs: compaction is
// bookkeeping, not model work, so it must not burn the turn's time budget.
// nil (one-shot mode, or a host with no per-turn clock) leaves the fold on the
// caller's context and budget as-is. See the Shaper.Pause field.
var CompactionPause func() (resume func())

// Harness is a running dun: the MCP manager + an agent Session over its tools.
type Harness struct {
	// recapsSince counts recaps applied since the last main round's stat.
	recapsSince atomic.Int32
	// procs samples background jobs' process groups (bgjob.go usage).
	procs procstat.Sampler
	// The last plain jobs read and its answer (jobs.go): a repeat that
	// answered the same is turned into a wait.
	jobsPollMu  sync.Mutex
	jobsPollSig string
	jobsPollOut string
	// The turn in flight's cancel and what its round has streamed (interrupt.go).
	interruptState
	// The session's worklog (worklog.go), read from its sidecar on first use.
	wlMu   sync.Mutex
	wl     *Worklog
	wlFile string
	// The session's task plan (plan.go), read from its sidecar on first use.
	planMu sync.Mutex
	plan   *planState

	mgr     *mcpmgr.Manager
	Session *agent.Session
	Tools   []mcpmgr.MCPTool
	store   *sessionStore
	client  agent.LLMRunner // kept for its retry policy (see turnRetryPolicy)
	onRetry func(RetryNote)
	// ticket is the id a fair-share proxy issued for the turn in flight, so a
	// turn-scope retry can present it and keep the place in line the earlier
	// attempt earned. Guarded because it is written from the request's
	// goroutine (the llm client's retry hook) and read from the turn's.
	ticketMu sync.Mutex
	ticket   string
	wake     chan struct{} // signals a driver to run a Continue turn (bg job done)
	wakes    serverWakes   // servers that may start a turn (serverwake.go)
	bgMu     sync.Mutex
	bgSeq    int
	bgRun    int            // background jobs still running
	bgJobs   map[int]*bgJob // every job this session started, by id (see bgjob.go)
	// bgLive is every job not yet finished, numbered or not: a command still
	// inside its grace has no id yet, and /jobs kill all and an interrupt
	// must still find it (see bgkill.go).
	bgLive map[*bgJob]struct{}

	// Sub-agents (see subagent.go and docs/decisions.md, "Sub-agents"). agMu guards both.
	// ownsMgr is false for a CHILD: it shares its parent's manager, and closing
	// that from a child would stop the servers out from under the parent.
	agMu  sync.Mutex
	agSeq int

	// rounds is what each model round cost, the open turn's totals and the
	// endpoint's prefill speed (roundstats.go). The root session's only.
	rounds     roundStats
	agents     map[int]*subAgent
	ownsMgr    bool
	parent     *Harness    // nil for a root
	agentID    int         // this harness's id within its parent; 0 for a root
	self       *subAgent   // this harness's own record IN its parent; nil for a root
	turnRounds turnRounds  // this turn's tool-call rounds, for the turn limit
	nudge      planNudge   // this turn's plan nudge (plannudge.go)
	idle       idleState   // changes since this turn's last check (idlecheck.go)
	learn      learnState  // resolved churn waiting for its lesson (learn.go)
	settle     settleState // the delayed end-of-turn report (settle.go)
	noteMu     sync.Mutex
	queue      []queued // messages not yet delivered to the model
	// userWaitCh is closed by Say, so a tool sitting in a wait can stop and let
	// the message reach the model now. Guarded by noteMu; see userWaiting.
	userWaitCh chan struct{}
	// endWaitCh is closed when a background job or a sub-agent's run ends, so
	// an exec waiting out its grace hands over at once. Guarded by noteMu; see
	// workEnded.
	endWaitCh chan struct{}
	// budgetNote is the budget notice waiting for the next model call (see
	// budget.go). Guarded by noteMu.
	budgetNote string
	// docsPrep is the proactive search's preparer, nil while no docs server
	// runs. The Session's Preparer wraps it (withBudgetNotice), so is never nil.
	docsPrep agent.NotificationPreparer

	// files is the built-in file toolset for the current working tree, and
	// filesKey what it was built from (see fileTools). Touched only under
	// turnMu, by rebuildTools.
	files    *filetools.Toolset
	filesKey string
	// sideCalls accumulates the per-kind stats for the non-turn LLM calls
	// (suggest, rephrase, commit, rescue) — see sidecall.go.
	sideCalls *sideCallStats
	// surfacing accumulates what proactive RAG appends and re-sends — see
	// surfacing.go.
	surfacing *surfacingStats
	// cache files each round's cache hit under the kind of build that preceded
	// it, and counts prefix rewrites by cause — see cachestats.go.
	cache *cacheStats
	// forcedToolCalls are tool calls queued by the host  that
	// should be injected into the next LLM response's tool_calls, so they
	// appear as if the model made them. Guarded by noteMu.
	forcedToolCalls []llm.ToolCall

	// Servers can start and stop mid-session (/rag on, /lsp off), which means
	// the tool set is not fixed at construction: srvMu guards the spec list and
	// the last-error map, and every change ends in applyTools rebuilding the
	// Session's tools, dispatcher, system prompt and doc preparer.
	cfg   Config
	srvMu sync.Mutex
	// builtinMu guards builtinDefs: dun's own tool definitions at the last
	// rebuild, switched off or not, for /mcp's built-in rows (builtins.go).
	builtinMu   sync.Mutex
	builtinDefs []llm.ToolDef
	specs       []Server                 // every configured server, running or not
	lastErr     map[string]string        // id → why its last start attempt failed
	missing     map[string]bool          // id → lastErr is a MissingError
	lastStart   map[string]time.Duration // id → how long its last start took
	// starting is the servers a start is in flight for, and since when: a
	// slow one (poly-lsp indexing a big repo takes a minute) is said to be
	// starting rather than off, and is not started a second time.
	starting map[string]time.Time
	// srvLogs is each server's activity log (serverlog.go); closed is closed
	// by Close, which ends the log's stderr readers.
	srvLogs serverLogs
	// startStop cancels a start in flight (startServer), with why: a
	// server switched off while it starts stops starting.
	startStop map[string]context.CancelCauseFunc
	// reloads counts reloadServers calls in flight, starts included: what
	// follows a reload (indexstate.go's retry) waits for none to be.
	reloads atomic.Int32
	// effort is /effort's level (effort.go), applied on every request.
	effort atomic.Value
	// thinking is what the server's model list says /effort default does
	// (effort.go); nil until it says.
	thinking atomic.Pointer[ServerThinking]
	// windowSrc is where the window in force came from (window.go).
	windowSrc string
	closed    chan struct{}
	closeOnce sync.Once
	// ingestNote is the last workspace ingest's summary, for /rag and the
	// ready hint, and ingestID the server it went to. Guarded by srvMu.
	ingestNote string
	ingestID   string
	// index is each server's index state (indexstate.go); indexSeen the
	// servers that sent an index notification this session; indexRetried
	// the ones dun has restarted with excludes once. Guarded by srvMu.
	index        map[string]*indexState
	indexSeen    map[string]bool
	indexRetried map[string]bool
	// indexAsked: servers whose not-a-project question was put this session.
	indexAsked map[string]bool
	// setup is each server's install-hint report (setupstate.go); setupSaid
	// the servers whose one line has been said. Guarded by srvMu.
	setup     map[string]*SetupReport
	setupSaid map[string]bool
	// exts are the loaded extensions and extErrs the manifests that did not
	// load (extension.go). Fixed at Start.
	exts    []Extension
	extErrs []ExtError
	hosted  map[string]hostedState // id → how the running shell took a shell-placed server
	// sessSw is this session's own server switches (mcpscope.go), saved in
	// the session's .mcp.json sidecar. Guarded by srvMu. reconMu orders the
	// reloads that apply a switch change.
	sessSw  map[string]SessionServer
	reconMu sync.Mutex
	// oauth is each OAuth server's login, kept across restarts of the server
	// so a cached token and a running login are not lost. Its own lock:
	// Servers() reads it while holding srvMu.
	oauthMu sync.Mutex
	oauth   map[string]oauthEntry
	// added is the /file grants as file toolsets (addedWriters), read by
	// each file call: a path the person adds in a turn is open to that
	// turn's next call, though the tool set waits for the turn's end.
	// Dropped by every edit of a paths file (editPathsFile).
	addedMu sync.Mutex
	added   *addedSets
	// shared is each server reached through dund's gateway: its grant
	// (servers_share.go). Guarded by srvMu.
	shared map[string]ShareGrant
	// shellShares are the grants the running shell's hosted servers use,
	// by server id (servers_share.go shareHosted). Guarded by srvMu.
	shellShares map[string]ShareGrant
	// ownMgr is a child's own instances (servers_share.go childOwn): an
	// agent instance, and for a child isolated in a tree of its own each
	// server keyed on {{workspace}}. own names them; every other server is
	// its parent's, in mgr. Set in Start, before any turn.
	ownMgr *mcpmgr.Manager
	own    map[string]bool
	// raglit's keys for this session, on the root (raglitauth.go).
	ragMu                            sync.Mutex
	ragSession, ragKey, ragBranchKey string
	// turnMu is held for the length of a turn. A server command arrives on
	// another goroutine (the -p reader), and swapping the Session's tools out
	// from under a running turn is a data race — so a rebuild that cannot take
	// the lock sets applyPending and the turn applies it on the way out.
	turnMu       sync.Mutex
	applyPending atomic.Bool
	// toolNames is the tool set's names as of the last rebuild, sorted.
	toolNames atomic.Pointer[[]string]
	// memory is the note store when the session has a RagLink, else nil.
	// Set by rebuildTools; read by /remember from another goroutine.
	memory atomic.Pointer[memoryStore]
	// memLinkOnce makes memLinkV, the notes' link to dund, once per harness
	// (a child uses its parent's).
	memLinkOnce sync.Once
	memLinkV    *memLink
	// strategies is the strategies and constraints store, beside memory
	// (notekinds.go); stratLink its link to dund. listed is the files the
	// standing list of the system prompt shows, as of the last rebuild.
	strategies    atomic.Pointer[memoryStore]
	stratLinkOnce sync.Once
	stratLinkV    *memLink
	// codeLinkOnce makes codeLinkV, the link to the code and docs indexes
	// (codesearch.go), once per harness (a child uses its parent's).
	codeLinkOnce sync.Once
	codeLinkV    *codeLink
	listed       atomic.Pointer[map[string]bool]
	// catalog is what each started server offers besides tools: skills and
	// prompts (skills.go). Guarded by srvMu.
	catalog map[string]mcpCatalog

	// The local skills as last seen by CheckSkills.
	skillMu    sync.Mutex
	skillSeen  bool
	skillSig   string
	skillNames []string

	// What syncSkillIndex last put in the skills index: SKILL.md → size+mtime.
	skillIdxMu sync.Mutex
	skillIdx   map[string]string
	skillDocs  map[string]skillHit // indexed file → its skill, for surfacing

	// The project's rules (rules.go): rulesMu guards the state file,
	// rulesIdxMu orders index syncs, rulesDocsMu guards rulesDocs — the
	// imported files in the rules index, for surfacing.
	rulesMu     sync.Mutex
	rulesIdxMu  sync.Mutex
	rulesDocsMu sync.Mutex
	rulesDocs   map[string]bool
	// trigMark is the CreatedAt of the newest entry the memory triggers have
	// looked at (memtrigger.go).
	trigMu   sync.Mutex
	trigMark int64
	// toolsInit is false until the first rebuild, so the initial tool set is
	// not announced as a CHANGE — the system prompt already describes it.
	toolsInit bool
	// loop is the run of consecutive identical tool calls, for the guard that
	// refuses one that cannot return anything new. See loopguard.go.
	loop loopGuard
	// recapNudged is the window size at the last recap reminder, so the same
	// line is not repeated on every chat round past the threshold.
	recapMu     sync.Mutex
	recapNudged int
	recapSeen   map[string]bool // suggestions already made, so none repeats
	// capped memoizes the clip by content, so the model and the UI get the same
	// text and one spill rather than two.
	capped map[string]string
	// execVerdicts is how each exec call ended, by tool call id, for the
	// stub a recap leaves (recapstub.go).
	execVerdicts map[string]string
	// Compaction counters. compactTurn resets each turn, so >1 is thrashing
	// rather than a busy session.
	compactMu   sync.Mutex
	compactTurn int
	// compactOverflow counts the folds THIS TURN that the overflow policy asked
	// for, so the thrash warning does not blame the budget for them.
	compactOverflow int
	compactLast     time.Time
	// Session-level counters for /context stats. Guarded by noteMu alongside
	// the queue, since they are incremented when items are drained from it.
	// meter is the chars-per-token ratio measured from the provider's own usage
	// reports. It is the Shaper's estimator AND the Session's, so the budget the
	// context is shaped to and the window size reported to the user are the same
	// number by construction. See internal/tokenmeter.
	meter tokenmeter.Meter
	// req measures the turn's requests for a status bar (reqstats.go); nil
	// when nothing listens.
	req *reqMeter
	// window is the endpoint's whole context, prompt and response together, or 0
	// when nobody could state one. Kept because the response's budget is the
	// window MINUS this round's prompt, which is a per-round subtraction the
	// startup-time shaping budget cannot stand in for.
	//
	// atomic because it is no longer written once at startup: a session that
	// began without a window adopts one mid-flight (see ensureWindow), from the
	// turn's goroutine, while /context reads it from the UI's.
	window atomic.Int64
	// windowAsks / windowSkip drive that retry: how many times the endpoint has
	// been asked since startup, and how many builds to let pass before asking
	// again. Touched only from the context builder, which is one goroutine.
	windowAsks int
	windowSkip int
	// windowRunner is the LLM, kept so the window can be re-asked. nil once
	// there is nothing left to ask (a window is known, or asking was given up).
	windowRunner any
	// Overflow counters for /context: rounds the endpoint ended for room, and
	// how many of those were answered by folding history. Guarded by noteMu
	// alongside the other session-level counters.
	overflowCuts        int
	overflowFolds       int
	systemTokens        int             // system prompt + tool schemas; exact when the endpoint can tokenize
	systemParts         SystemBreakdown // the same total, broken down and labelled exact/estimated
	forcedCallsTotal    int             // total forced tool calls injected this session
	notificationsLifted int             // notifications delivered via liftQueued (smuggled)
}

func (h *Harness) SessionStats() (systemTokens, forcedCalls, notificationsLifted int) {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	return h.systemTokens, h.forcedCallsTotal, h.notificationsLifted
}

// startBackground lives in bgjob.go, with the job registry and the monitor.

// Start spawns the servers, waits for tool discovery, and builds the Session.
func Start(ctx context.Context, cfg Config) (*Harness, error) {
	// The servers are the built-ins (or a caller's Go-level ones) under the
	// layered config files, under this session's own switches (mcpscope.go)
	// and the --rag/--lsp flags. A child takes its parent's resolved list
	// as given: it shares the parent's manager and switches nothing.
	servers := cfg.Servers
	var sessSw map[string]SessionServer
	extDir := cfg.ConfigDir
	if extDir == "" {
		extDir = cfg.Workspace
	}
	// Pinned: every reader of ConfigDir falls back to Workspace, and
	// Rehoist (/isolate) moves Workspace into a worktree; the config, and
	// the directory /isolate off goes back to, stay the checkout's.
	cfg.ConfigDir = extDir
	// Extensions: the bundled ones and the user's and project's manifests
	// (extension.go). Their servers join the base the files layer over.
	exts, extErrs := LoadExtensions(extDir)
	for _, e := range extErrs {
		log.Printf("dun: extension %v", e)
	}
	if cfg.Parent == nil {
		dir := extDir
		base := cfg.Servers
		if base == nil {
			base = DefaultServers(cfg.Workspace, cfg.RaglitHome)
		}
		base = extServers(base, exts)
		sessSw = LoadSessionServers(cfg.SessionFile)
		var err error
		servers, err = resolveServers(dir, cfg.Workspace, cfg.RaglitHome, base, sessSw, cfg.AutostartOverride)
		servers = withoutDirServers(servers, cfg)
		if err != nil {
			return nil, err
		}
	}
	// A CHILD shares its parent's manager. Same worktree root means the same
	// servers answer the same questions, so spawning a second set would pay for
	// a second poly-lsp index and a second raglit ingest to get identical
	// answers. It also must not start or stop them: that is the parent's.
	//
	// The exception is the child's own instances (childOwn): a server with
	// instance agent, and, for a child isolated in a tree of its own, each
	// server keyed on {{workspace}}. They run in a manager of the child's,
	// under the same ids, so the tool names the model sees do not change.
	mgr, ownsMgr := cfg.Parent.managerFor()
	var own map[string]bool
	if cfg.Parent != nil {
		servers, own = childOwn(cfg.Parent, cfg, servers)
	}
	store, err := openSessionStore(cfg.SessionFile)
	if err != nil {
		if ownsMgr {
			mgr.Close()
		}
		return nil, fmt.Errorf("dun: open session: %w", err)
	}
	store.onNotify = cfg.OnNotify
	h := &Harness{mgr: mgr, store: store, client: cfg.Client,
		onRetry: cfg.OnRetry, wake: make(chan struct{}, 16),
		cfg: cfg, specs: servers, sessSw: sessSw, exts: exts, extErrs: extErrs, lastErr: map[string]string{}, lastStart: map[string]time.Duration{}, starting: map[string]time.Time{}, closed: make(chan struct{}),
		ownsMgr: ownsMgr, parent: cfg.Parent, agentID: cfg.AgentID,
		sideCalls: &sideCallStats{}, surfacing: &surfacingStats{}, cache: &cacheStats{}}
	// /effort's saved level (effort.go), unless this harness was given its
	// own: a child spawned with an effort keeps it, and does not save it.
	if cfg.Effort != "" {
		h.effort.Store(cfg.Effort)
	} else {
		h.effort.Store(LoadEffort())
	}
	if cfg.Parent == nil {
		applyContextConfig(savedContext(cfg.Endpoint)) // /context config, saved for this model (window.go)
	}
	// A resumed session's history is not this session's to act on: the
	// memory triggers look only at what arrives from here on.
	h.markTriggerScan()
	h.settleResume() // a settle owed when the session stopped (settle.go)
	// Saved output readable by path again, after a reboot wiped /tmp (isotmp.go).
	h.restoreIsoTmp()
	// Every response the engine's clients get reaches the root session's
	// round stats, and its stats ledger (roundstats.go). A child shares the
	// parent's meter and leaves the hook to it.
	if cfg.Parent == nil && cfg.Spend != nil {
		h.rounds.ledger = cfg.StatsLedger
		cfg.Spend.SetRoundHook(h.noteRound)
	}

	// Server→client push. Only the OWNER registers: a child shares its
	// parent's manager, and registering again would replace the parent's sink
	// with one pointed at the child's conversation. Must precede any
	// StartServer — handlers attach at spawn.
	if ownsMgr {
		mgr.SetNotificationHandler(h.noteServerNotification)
	}
	if len(own) > 0 {
		h.ownMgr, h.own = mcpmgr.NewManager(), own
		h.ownMgr.SetNotificationHandler(h.noteServerNotification)
	}

	// Autostart is best-effort by design: a missing binary or a server that
	// refuses to run is worth SAYING (it lands in Servers()[i].Err, which the
	// UI reports), but it is not worth refusing to start the session over. The
	// user can fix the binary and /rag on without losing the session.
	//
	// Every server starts in the background, all at once. The session waits
	// for the built-ins only up to serverStartGrace, and for an added server
	// (often remote, now and then behind a login gate that burns the whole
	// discovery timeout) not at all: a built-in that answers by then is in
	// the first tool set, and one that does not — poly-lsp indexing a big
	// repo took a minute — does not hold the session up. Its tools join the
	// tool set when it is up (between turns, applyTools), and
	// OnServerStarted reports it. Servers() says it is starting until then.
	var builtins sync.WaitGroup
	for _, s := range servers {
		if !s.Autostart || (!ownsMgr && !own[s.ID]) || s.Via == ViaShell { // the shell starts it
			continue
		}
		h.markStarting(s.ID)
		wg := &sync.WaitGroup{}               // an added server's: nobody waits on it
		if builtinServer(s.ID) || own[s.ID] { // a child's own: its first turn wants them
			wg = &builtins
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.startServer(ctx, s)
			var miss *MissingError
			if err != nil && !errors.As(err, &miss) { // not installed: the UI says it, once
				log.Printf("dun: %s did not start: %v", s.ID, err)
			}
			if cfg.OnServerStarted != nil && ownsMgr { // the session's servers, not a child's
				cfg.OnServerStarted(s.ID, err)
			}
		}()
	}
	waitGrace(&builtins, serverStartGrace)
	// Carry the client's own retry narration out to the UI. Without this the
	// waiting is only ever logged, and a TUI's log is not on screen.
	applyRetryPolicy(cfg.Client)
	wireRetry(cfg.Client, cfg.OnRetry, h.noteTicket)

	// The window is subtracted from all session long: the prompt's budget on
	// every build, the response's cap on every round. It is ASKED for here and,
	// when the endpoint cannot say yet, asked again as the session runs — see
	// ensureWindow for why one missed request must not disarm a whole session.
	h.window.Store(int64(h.contextWindow(ctx, cfg.Client)))
	if h.windowTokens() == 0 {
		h.windowRunner = cfg.Client
	}

	// Context shaping. The Shaper's algorithm is a LADDER, and every rung needs
	// its own policy field — setting BudgetTokens alone disables the cheap rungs
	// and leaves only the expensive one:
	//
	//   0. pristine tail   PreserveLastMessages / PreserveLastToolCalls
	//   1. LOD stubs       LODTruncateAboveChars  (render-time; no writes)
	//   2. compaction      runs ONLY if LOD still leaves it over budget
	//
	// Measured the hard way: a policy with only BudgetTokens set skipped rungs 0
	// and 1, so every build compacted and preserved nothing — 11 entries
	// survived from 97 tool calls, and rewriting the prefix each turn
	// invalidated the KV cache for 8.5x the processed tokens and 3x the wall
	// clock.
	//
	// LODTruncateAboveChars is the important one here: tool results ARE the
	// context in a coding agent (measured: 122k of a 180k window, one gradle log
	// 19.8k chars). Stubbing older oversized results is render-time and lossless
	// on disk, and it is what keeps compaction from ever being needed.
	shaper := &agent.Shaper{
		Store:  store,
		Runner: cfg.Client,
		// Not agentkit's CharsByFour default: on this endpoint's own traffic
		// that is 31% low, which puts the shape target above the whole window
		// and makes every rung below unreachable. See internal/tokenmeter.
		Estimate: &h.meter,
		Policy:   shaperPolicy(h.windowTokens()),
		// Compaction is bookkeeping, not model work: stop the turn's budget
		// clock for the fold so a slow endpoint + a long compaction cannot run
		// the turn past its budget mid-fold and cancel the worker with a bare
		// "context canceled". Set by the host (cmd/dun) from its turn clock;
		// nil (one-shot / no clock) means the fold runs on the budget as-is.
		Pause: CompactionPause,
		// Which end of an old, oversized result its stub keeps. exec output
		// opens with what failed and closes with the verdict and [exit:N]; a
		// head-only stub drops the line that says whether it passed (aw4
		// measured this on its suite runs). Every other tool keeps the head.
		ToolLOD: toolLOD(),
		// Stub the older copy of a read made again — once, just before a fold,
		// when the prefix is rewritten anyway. See rehead.go.
		Rehead: h.rehead,
		// A fold thinks nothing and is told the session's state and what is
		// in flight; its summary is sized by agentkit to the fold (fold.go).
		SummaryOpts: h.foldOpts(),
		TaskFunc:    h.foldTask,
		// What a fold deletes stays searchable in the session's raglit
		// branch (raglitauth.go); nil without a raglit that has keys.
		Index: lateSessionIndex{h},
	}
	// A sub-agent's first user message IS its task (the parent's brief), so it
	// must survive folds. Not for a root session: its first message may be
	// "hi" or a pasted log, and a pinned entry rides every round for good.
	shaper.Policy.PinFirstUser = h.isChild()
	sess := &agent.Session{
		SessionID: "dun",
		// dun writes the {OUTPUT} convention itself, shorter (systemSlots).
		OmitSlotInstructions: true,
		OnCompactionComplete: h.noteCompaction,
		Store:                store,
		Runner:               h.teeTurn(cfg.Client, cfg.OnReasoning),
		OnAssistantToken:     cfg.OnToken,
		OnUsage:              h.noteUsage,
		MaxTurns:             h.turnLimit(),
		Build:                measuredBuild(shaper, h),
		ToolFormat:           toolFormat(),
		// The same estimator the Shaper uses, so `Active` and the budget cannot
		// disagree about how big the context is.
		Estimate: &h.meter,
		// The caps applyGeneration writes on every round. Non-nil from the start
		// so the Session has something to copy; the values arrive with the first
		// build, which runs before the first request.
		ChatOpts: &llm.ChatOpts{},
		// finish_reason=length: decide between shedding history and telling the
		// model what it hit. See overflow.go.
		OnOverflow: h.onOverflow,
		// Where the turn would end: a check, or the open plan (idlecheck.go).
		OnIdle: h.onIdle,
	}
	// $DUN_HARNESS in exec's environment, and the file it names written
	// before the first request (harnessinfo.go).
	h.cfg.Exec = h.withExecEnv(h.cfg.Exec)
	h.writeHarnessInfo()
	// Tools, Dispatch, System and Preparer all depend on WHICH servers are
	// running, and that changes mid-session (/rag on, /lsp off). applyTools
	// owns those four fields; nothing else may set them.
	//
	// The Session is published under turnMu: a server still starting in the
	// background rebuilds the tools from its own goroutine, under the same
	// lock, and must see either no Session or this one.
	h.turnMu.Lock()
	h.Session = sess
	h.applyPending.Store(false)
	h.rebuildTools()
	h.turnMu.Unlock()
	if h.applyPending.Load() { // a server came up while the lock was held
		h.applyTools()
	}
	// The workspace into dund's code and docs indexes, in the background
	// (codesearch.go); again at each turn end.
	h.codeReindex()
	// Children of a resumed session are gone as processes but their transcripts
	// are not. Register them (restarted, in a session that persists; else
	// stopped) and SAY so — a model that spawned an agent last session must
	// not assume it is still working.
	if !ownsMgr {
		return h, nil // a child does not have children: depth stays at 1
	}
	// Once, at start: which children came back, and how.
	if note := h.restoredAgentsNote(h.restoreAgents()); note != "" {
		h.Aside(note)
		h.agentsChanged()
	}
	// What dun is in this session, for dun session <id> info (harnessinfo.go).
	// Not on a goroutine: a write landing after the caller closed the session
	// (a test's temp dir removed) fails its cleanup.
	h.writeHarnessInfo()
	return h, nil
}

// Ask injects a user message and runs the tool loop to completion.
//
// Sending a message is also how a user RESUMES a session that died on the
// provider: prepareTurn runs first, so an interrupted tool call is paired off
// before the new message is appended (appending it between a call and its result
// would be the one order the provider rejects), and the turn then retries the
// transport for as long as the policy allows.
func (h *Harness) Ask(ctx context.Context, task string) (agent.TurnResult, error) {
	ctx, end := h.interruptible(ctx)
	defer end()
	h.clearBudgetNotes()
	h.settleBegin(true)
	h.prepareTurn(ctx)
	h.workInput(task)
	h.store.publish(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindUser, Content: task, CreatedAt: time.Now().UnixNano(),
	})
	res, err := h.turnThenTriggers(ctx)
	h.settleEnd(true, false, err)
	return h.finishInterrupt(ctx, res, err)
}

// turnThenTriggers runs a turn, then — in the background, once the turn has
// let go of turnMu — the memory triggers over what it added.
func (h *Harness) turnThenTriggers(ctx context.Context) (agent.TurnResult, error) {
	res, err := h.runTurnWithRescue(ctx, h.Session.Turn)
	// Held churn episodes become candidate lessons once the task verifies
	// (learn.go): after the turn, so no request of the turn waits on it.
	h.learnAfterTurn(ctx, err)
	// The turn's edits into the code and docs indexes (codesearch.go), with
	// or without an error: a failed turn can have written files too.
	h.codeReindex()
	if err == nil {
		h.workReply(res.Reply) // the worklog's open task (worklog.go)
		// Not the turn's ctx: the caller cancels it once the turn returns,
		// which is before the fork has started (live, every trigger failed
		// "context canceled").
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), triggerTimeout)
		go func() {
			defer cancel()
			if msg := h.checkMemoryTriggers(fctx); msg != "" {
				h.Aside("[memory] " + msg)
				if h.cfg.OnMemoryTrigger != nil {
					h.cfg.OnMemoryTrigger(msg)
				}
			}
		}()
	}
	return res, err
}

// Close shuts down the MCP servers.
// Close stops this harness's children, then its servers.
//
// Children first, and unconditionally: a session that ends KILLS them (see
// docs/decisions.md, "Sub-agents"), and a child left running after its parent's servers are
// gone would keep working against tools that no longer answer.
func (h *Harness) Close() {
	h.markClosed()
	h.settleClose()
	h.closeAgents()
	// The shell this harness owns. A child's is its own (see childConfig), so
	// closing it here does not touch the parent's.
	if hs, ok := h.cfg.Exec.(*HostShell); ok {
		hs.Close()
	}
	// A child does not own the manager it borrowed. Closing it here would stop
	// the servers out from under the parent that lent them. Its own instances
	// are its own: they stop with it (a shared one's client is released).
	if h.ownsMgr {
		h.mgr.Close()
	}
	if h.ownMgr != nil {
		h.ownMgr.Close()
	}
}

// managerFor gives a child its parent's manager, or a root a fresh one. The
// bool is whether the caller OWNS it, which is what decides who may close it.
// A child's own instances are in a manager of its own (Harness.ownMgr).
func (h *Harness) managerFor() (*mcpmgr.Manager, bool) {
	if h == nil {
		return mcpmgr.NewManager(), true
	}
	return h.mgr, false
}

// Reset clears the session store, starting a fresh conversation log.
func (h *Harness) Reset() { h.store.Reset() }

// ExecBackend returns the current exec backend.
func (h *Harness) ExecBackend() ExecBackend {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.cfg.Exec
}

// Workspace returns the workspace directory.
func (h *Harness) Workspace() string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.cfg.Workspace
}

// Mounts are the extra local paths this session was started with — the go.mod
// replace directives and any dun.json `mounts` entries, already resolved to
// absolute source paths.
//
// Exposed because a worktree created MID-SESSION needs the same list the one
// created at startup got. Reusing the resolved list rather than re-loading it
// is what keeps the symlinks in the worktree parent and the Docker volume
// mounts describing the same set: two loads of the same config can disagree if
// a file changed underneath, and then a build works in one isolation tier and
// not the other.
func (h *Harness) Mounts() []MountSpec {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.cfg.ExtraMounts
}

// Worktree returns the current worktree (may be nil).
func (h *Harness) Worktree() *Worktree {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.cfg.Worktree
}

// AskUser puts a question to the human attached to this session and returns
// their answer. It is the ask_user tool's path, exposed for the parts of the
// COMMAND layer that need a decision (/worktree commit showing a message before
// it writes it) rather than a turn.
//
// CALLER BEWARE: this blocks until the human answers, and in the -p/TUI engine
// the goroutine that reads control commands is the same one that reads answer
// events. Calling this from a control-command callback deadlocks — run it on
// its own goroutine. See setCtrlCmd in cmd/dun/main.go.
func (h *Harness) AskUser(ctx context.Context, question string, options []string, recommended string) (string, error) {
	h.srvMu.Lock()
	ask := h.cfg.Ask
	h.srvMu.Unlock()
	if ask == nil {
		return "", fmt.Errorf("nobody to ask — this session has no human attached")
	}
	var rec []string
	if recommended != "" {
		rec = []string{recommended}
	}
	return ask(ctx, question, options, false, rec)
}

// Home is the directory the session works in when it is in place: the
// developer's checkout (Config.ConfigDir), not a worktree it moved into.
// /isolate off goes back here.
func (h *Harness) Home() string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.configDir()
}

// NoDir reports a session attached to no directory (dun --no-dir).
func (h *Harness) NoDir() bool { return h.cfg.NoDir }

// ToolNames lists the agent's tool names (MCP tools + the built-in exec), sorted.
//
// Read from the snapshot rebuildTools leaves, not from Session.Tools: a
// server that comes up late rebuilds the tools on its own goroutine while
// a client is asking.
func (h *Harness) ToolNames() []string {
	if p := h.toolNames.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}
