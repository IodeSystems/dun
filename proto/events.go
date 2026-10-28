package proto

// Event payloads, one struct per type. The field names are the engine's
// existing ones (cmd/dun main.go), so moving the engine onto these structs
// changes no line a client reads.
//
// Diagnostic blocks that only /context reads (side_calls, surfacing, cache)
// stay open maps: their keys change with the counters behind them, and no
// client branches on them.

func init() {
	event[Token]("token")
	event[Reasoning]("reasoning")
	event[Message]("message")
	event[ToolCall]("tool_call")
	event[ToolResult]("tool_result")
	event[Done]("done")
	event[Request]("request")
	event[Usage]("usage")
	event[ContextCost]("context_cost")
	event[Spend]("spend")
	event[Ask]("ask")
	event[Mount]("mount")
	event[Error]("error")
	event[Retry]("retry")
	event[Queued]("queued")
	event[Notification]("notification")
	event[Recap]("recap")
	event[Compaction]("compaction")
	event[Session]("session")
	event[History]("history")
	event[Workspace]("workspace")
	event[Ready]("ready")
	event[ServerEvent]("server")
	event[ControlEvent]("control")
	event[Catalog]("catalog")
	event[PromptText]("prompt")
	event[ResetEvent]("reset")
	event[Agents]("agents")
	event[Jobs]("jobs")
	event[AgentHistory]("agent_history")
	event[AgentTold]("agent_told")
	event[Suggestions]("suggestions")
	event[Exit]("exit")
	event[UserEcho]("user")
	event[ImageRefused]("image_refused")
	event[Turn]("turn")
	event[Answered]("answered")
	event[Settings]("settings")
	event[Commands]("commands")
	event[Models]("models")
	event[McpTools]("mcp_tools")
	event[McpLogin]("mcp_login")
	event[Switches]("switches")
	event[SkillFile]("skill_file")
	event[Extensions]("extensions")
	// The protocol's own.
	event[Ack]("ack")
	event[Snapshot]("snapshot")
}

// --- a turn ---

// Token is a streamed fragment of the assistant's reply.
type Token struct {
	Text string `json:"text"`
}

// Reasoning is a streamed fragment of the model's reasoning — its thinking
// before the reply or the tool call of a round. Deltas, like Token, and only
// the turn's own: a sub-agent's or a side call's thinking is not sent. A
// round's thinking ends when its first token or tool_call arrives, or the
// turn ends (done, error); there is no event of its own for that. Thinking
// from finished rounds comes back in history as items of kind "reasoning".
type Reasoning struct {
	Text string `json:"text"`
}

// Message is a whole reply, sent when the turn's text is final.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ToolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type ToolResult struct {
	Tool   string `json:"tool"`
	Result string `json:"result"`
}

// Done ends a turn. Reason is empty for a turn that finished, and
// "interrupted" for one the user stopped (see Interrupt). Detail, on an
// interrupted turn, lists the jobs it started that are still running and how
// to stop them ("still running: #3 go test ./... (2m) — /jobs kill 3 …").
type Done struct {
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Request is one turn round's speed and size (dun/reqstats.go), for a status
// bar. Phase "start" when the request is sent, "live" while it streams (at
// most about 4 a second), "done" with the provider's figures, and "ctx" when
// a recap or compaction moved the context. The turn's own rounds only: side
// calls and sub-agents are not counted. A rate is 0 (absent) until measured;
// *_server says it is the server's own timing (llama.cpp). Exact: gen is the
// provider's count, not the live estimate. CacheReported: the provider sent
// the cached-tokens field, so cached 0 means 0% rather than unknown.
type Request struct {
	Phase         string  `json:"phase"`
	TTFTMS        int64   `json:"ttft_ms,omitempty"`
	Prompt        int     `json:"prompt,omitempty"`
	Cached        int     `json:"cached,omitempty"`
	CacheReported bool    `json:"cache_reported,omitempty"`
	PPS           float64 `json:"pp,omitempty"`
	PPSServer     bool    `json:"pp_server,omitempty"`
	Gen           int     `json:"gen,omitempty"`
	TGS           float64 `json:"tg,omitempty"`
	TGSServer     bool    `json:"tg_server,omitempty"`
	Exact         bool    `json:"exact,omitempty"`
	Ctx           int     `json:"ctx,omitempty"`
	Window        int     `json:"window,omitempty"`
}

// SystemCost is what the context costs before the conversation.
type SystemCost struct {
	SystemTokens int           `json:"system_tokens"`
	SystemExact  bool          `json:"system_exact"`
	SystemPrompt int           `json:"system_prompt"`
	SystemShared int           `json:"system_shared"`
	SystemParts  []ContextPart `json:"system_parts"`
	// The project rules block's share of SystemPrompt, and its /rules mode.
	SystemRules     int    `json:"system_rules,omitempty"`
	SystemRulesMode string `json:"system_rules_mode,omitempty"`
	// SystemFiles are the context files in SystemRules, each estimated.
	SystemFiles []ContextPart `json:"system_files,omitempty"`
}

type ContextPart struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// Window is how the context window is divided (dun.WindowBudget).
type Window struct {
	Window         int     `json:"window"`
	WindowReserved int     `json:"window_reserved"`
	WindowCap      int     `json:"window_cap"`
	PromptBudget   int     `json:"prompt_budget"`
	PromptTokens   int     `json:"prompt_tokens"`
	PromptOverhead int     `json:"prompt_overhead"`
	CharsPerToken  float64 `json:"chars_per_token"`
	RatioMeasured  bool    `json:"ratio_measured"`
	RatioRounds    int     `json:"ratio_rounds"`
	WindowCuts     int     `json:"window_cuts"`
	WindowFolds    int     `json:"window_folds"`
}

// Usage is a turn's accounting, sent before done.
type Usage struct {
	Total               int `json:"total"`
	Active              int `json:"active"`
	Cached              int `json:"cached"`
	Processed           int `json:"processed"`
	Generated           int `json:"generated"`
	Turns               int `json:"turns"`
	ForcedCalls         int `json:"forced_calls"`
	NotificationsLifted int `json:"notifications_lifted"`
	SystemCost
	Window
	SideCalls map[string]map[string]any `json:"side_calls,omitempty"`
	Surfacing map[string]any            `json:"surfacing,omitempty"`
	Cache     map[string]any            `json:"cache,omitempty"`
	// Spend is the session's money and limits as of this turn's end. Nil when
	// no meter is wired.
	Spend *Spend `json:"spend,omitempty"`
	// TurnStats is what this turn cost; nil from an engine that does not say.
	TurnStats *TurnStats `json:"turn_stats,omitempty"`
}

// TurnStats is one turn's cost: every model round between its start and its
// end, sub-agents and side calls included (dun/roundstats.go). The timings
// fields are llama.cpp's own report and are set only when Timed rounds had
// one; Cost only when Costed rounds reported one. PrefillTPS is the
// endpoint's measured prompt-processing speed (tokens a second, the median of
// PrefillRounds recent rounds), 0 when unmeasured.
type TurnStats struct {
	Rounds     int   `json:"rounds"`
	WallMS     int64 `json:"wall_ms"`
	ModelMS    int64 `json:"model_ms"`
	Prompt     int   `json:"prompt"`
	Cached     int   `json:"cached"`
	CacheWrite int   `json:"cache_write,omitempty"`
	Completion int   `json:"completion"`

	Timed    int     `json:"timed,omitempty"`
	PromptN  int     `json:"prompt_n,omitempty"`
	PromptMS float64 `json:"prompt_ms,omitempty"`
	GenN     int     `json:"gen_n,omitempty"`
	GenMS    float64 `json:"gen_ms,omitempty"`

	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"cur,omitempty"`
	Costed   int     `json:"costed,omitempty"`
	Mixed    bool    `json:"mixed,omitempty"`

	SubAgents int `json:"sub_agents,omitempty"`

	PrefillTPS    float64 `json:"prefill_tps,omitempty"`
	PrefillRounds int     `json:"prefill_rounds,omitempty"`
}

// Spend is what the gateway reported about money and limits: the session's
// total, the last turn's, the key's budget and the latest rate-limit windows.
// Sent with every usage event, and on its own as "spend" when a budget poll
// lands between turns.
//
// A value the endpoint did not report is ABSENT (a nil pointer, an empty list),
// never 0: a provider that says nothing about money must not read as free.
type Spend struct {
	// Currency of Session and LastTurn: "USD", "credits", or "" when the
	// source named none. Mixed: some costs came in another currency.
	Currency string `json:"currency,omitempty"`
	Mixed    bool   `json:"mixed,omitempty"`
	// Session is the summed cost of every response that reported one; nil when
	// none did. Calls / CostedCalls say how much of the session that covers.
	Session     *float64 `json:"session_cost,omitempty"`
	Calls       int      `json:"calls"`
	CostedCalls int      `json:"costed_calls"`
	// The session's tokens over all calls: the prompt, of it the cached part
	// (CacheReported: the provider said so at least once), and the completion.
	Prompt        int  `json:"prompt_tokens,omitempty"`
	Cached        int  `json:"cached_tokens,omitempty"`
	Completion    int  `json:"completion_tokens,omitempty"`
	CacheReported bool `json:"cache_reported,omitempty"`
	// LastTurn is the cost since the turn before it ended (side calls between
	// turns included); nil when none of those calls reported one.
	LastTurn       *float64 `json:"last_turn_cost,omitempty"`
	LastTurnCalls  int      `json:"last_turn_calls"`
	LastTurnCosted int      `json:"last_turn_costed"`
	// Key is the key's budget; nil when not known.
	Key *KeyBudget `json:"key,omitempty"`
	// KeyUnsupported: the endpoint exposes no key budget. KeyError: the last
	// read failed (the last good Key, if any, is still sent).
	KeyUnsupported bool   `json:"key_unsupported,omitempty"`
	KeyError       string `json:"key_error,omitempty"`
	// Limits are the rate-limit windows the latest response reported.
	Limits   []RateLimit `json:"limits,omitempty"`
	LimitsAt string      `json:"limits_at,omitempty"` // RFC 3339

	// Ledger is this machine's spend over time, every session and sub-agent
	// (dun/ledger.go); nil when no ledger is wired.
	Ledger *SpendLedger `json:"ledger,omitempty"`
}

// KeyBudget is a key's spend against its budget.
type KeyBudget struct {
	Spend     float64  `json:"spend"`
	Limit     *float64 `json:"limit,omitempty"`     // nil: no limit, or not reported
	Remaining *float64 `json:"remaining,omitempty"` // nil: not reported
	ResetAt   string   `json:"reset_at,omitempty"`  // RFC 3339; "" not reported
	Interval  string   `json:"interval,omitempty"`  // "30d", "monthly"; "" not reported
	Currency  string   `json:"currency,omitempty"`
	Source    string   `json:"source"`          // litellm, litellm-headers, openrouter
	AsOf      string   `json:"as_of,omitempty"` // RFC 3339, when it was read
}

// RateLimit is one rate-limit window.
// SpendLedger is the ledger's totals, each by currency ("" = not stated).
type SpendLedger struct {
	Today   map[string]float64            `json:"today,omitempty"`
	Week    map[string]float64            `json:"week,omitempty"`
	Month   map[string]float64            `json:"month,omitempty"`
	Year    map[string]float64            `json:"year,omitempty"`
	ByModel map[string]map[string]float64 `json:"by_model,omitempty"` // this month
	// Last24h is the session key's trailing 24 hours: the rate its budget is
	// measured against.
	Last24h map[string]float64 `json:"last_24h,omitempty"`
	Entries bool               `json:"entries,omitempty"`
}

type RateLimit struct {
	Name      string `json:"name"`                // requests, tokens, input tokens, output tokens
	Limit     *int64 `json:"limit,omitempty"`     // nil: not reported
	Remaining *int64 `json:"remaining,omitempty"` // nil: not reported
	ResetAt   string `json:"reset_at,omitempty"`  // RFC 3339; "" not reported
}

// ContextCost is the system cost at startup, before any turn. The window is
// absent when the estimate lands before the harness exists.
type ContextCost struct {
	SystemCost
	*Window
}

// Ask is a question for the human; the answer command replies.
type Ask struct {
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Multi       bool     `json:"multi"`
	Recommended string   `json:"recommended,omitempty"` // the first of RecommendedAll, for older clients
	// RecommendedAll is every option the model recommends: one, or for multi
	// the ones to check (any number).
	RecommendedAll []string `json:"recommended_all,omitempty"`
}

// Mount asks to mount a host path into the container; answered yes or no.
type Mount struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

// Error is a failed turn, or a failed start when fatal.
type Error struct {
	Error string `json:"error"`
	Fatal bool   `json:"fatal"`
}

// Retry is a provider call being retried, recovered, or given up on.
type Retry struct {
	Scope       string `json:"scope"`
	Kind        string `json:"kind"`
	Attempt     int    `json:"attempt"`
	Status      int    `json:"status"`
	DelayMS     int64  `json:"delay_ms"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	BudgetMS    int64  `json:"budget_ms"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail"`
	Text        string `json:"text"`
	ServerAsked bool   `json:"server_asked"`
	// Only when the provider reported its queue.
	Capacity int    `json:"capacity,omitempty"`
	InFlight int    `json:"in_flight,omitempty"`
	Waiting  int    `json:"waiting,omitempty"`
	Queue    string `json:"queue,omitempty"`
}

// Queued is a user message taken mid-turn, for the model's next tool result.
type Queued struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// Notification is a background note, or (kind "docs") what proactive search
// surfaced.
type Notification struct {
	Text      string   `json:"text,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Found     int      `json:"found,omitempty"`
	Surfaced  int      `json:"surfaced,omitempty"`
	Docs      []DocHit `json:"docs,omitempty"`
	Bytes     int      `json:"bytes,omitempty"`
	Tokens    int      `json:"tokens,omitempty"`
	LatencyMS int64    `json:"latency_ms,omitempty"`
}

type DocHit struct {
	Title  string  `json:"title"`
	ID     string  `json:"id"`
	Line   string  `json:"line"`
	Score  float64 `json:"score"`
	Inline bool    `json:"inline"`
	// Where it came from and why it won; absent from an older engine.
	Kind       string  `json:"kind,omitempty"`  // skill, memory, rule, mcp-doc, doc, attached doc
	Path       string  `json:"path,omitempty"`  // ~ for $HOME, relative inside the workspace
	Index      string  `json:"index,omitempty"` // the raglit index
	Root       string  `json:"root,omitempty"`  // a skill's root
	Candidates int     `json:"candidates,omitempty"`
	RunnerUp   float64 `json:"runner_up,omitempty"`
	// What the surfacing rule read; absent from an engine before the
	// similarity floor, or for a raglit before e66a3df.
	Similarity  float64 `json:"similarity,omitempty"`    // the hit's cosine
	Matched     int     `json:"matched_terms,omitempty"` // query words it holds
	QueryTerms  int     `json:"query_terms,omitempty"`   // of this many
	RunnerUpSim float64 `json:"runner_up_sim,omitempty"` // the runner-up's cosine
	Source      string  `json:"source,omitempty"`        // the above on one line, for display
	// "sim 0.71 · 3/4 terms · +0.07 over runner-up (of 4)", "only candidate ·
	// sim 0.66 · 2/3 terms"; from an older engine "only candidate · score …"
	// or "N× the runner-up (of K) · score …"
	Why string `json:"why,omitempty"`
}

// Recap is a span of the conversation rewritten shorter.
type Recap struct {
	Entries int    `json:"entries"`
	Chars   int    `json:"chars"`
	Note    string `json:"note"`
	Detail  string `json:"detail"`
}

// Compaction is history folded into a summary.
type Compaction struct {
	Text          string `json:"text"`
	Subsumed      int    `json:"subsumed"`
	TokensBefore  int    `json:"tokens_before"`
	TokensAfter   int    `json:"tokens_after"`
	Turn          int    `json:"turn"`
	SinceLastSecs int    `json:"since_last_secs"`
	Cause         string `json:"cause"`
}

// --- the session ---

// Session names the conversation; resumed counts the entries loaded.
type Session struct {
	ID      string `json:"id"`
	Resumed int    `json:"resumed"`
}

// History is the conversation so far, for a client to render as scrollback.
// It REPLACES what the client shows rather than adding to it: a client that
// got a snapshot also gets the history a respawned engine sends.
type History struct {
	Items []HistoryItem `json:"items"`
}

// HistoryItem mirrors dun.HistoryItem.
type HistoryItem struct {
	Kind    string         `json:"kind"`
	Content string         `json:"content,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	CallID  string         `json:"call_id,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
}

// Workspace is the worktree the session runs in.
type Workspace struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// Ready is sent once the engine takes commands.
type Ready struct {
	Tools   []string      `json:"tools"`
	Servers []ServerState `json:"servers"`
	Hint    string        `json:"hint"`
}

type ServerState struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	Auto    bool   `json:"auto"`
	Tools   int    `json:"tools"`
	Err     string `json:"err,omitempty"`
	// Missing: Err says the command is not installed — a setup step, not a
	// failure.
	Missing bool `json:"missing,omitempty"`
	// Auth is a remote server's credentials: its OAuth login state
	// ("oauth: logged in as … until …", "oauth: needs login …"), "static
	// bearer", "static header" or "none".
	Auth string `json:"auth,omitempty"`
	// The switches (dun mcpscope.go): Global, Project and Session are "on",
	// "off" or "" (unset); From is the scope that decided Auto. Via is
	// "direct" or "shell" ("" for a built-in), ViaGlobal/ViaProject/
	// ViaSession its value at each scope, ViaFrom the deciding scope.
	// Detail is what it is doing now, as one line.
	Via        string `json:"via,omitempty"`
	Proto      string `json:"proto,omitempty"` // "stdio", "https" or "http"
	Global     string `json:"global,omitempty"`
	Project    string `json:"project,omitempty"`
	Session    string `json:"session,omitempty"`
	From       string `json:"from,omitempty"`
	ViaGlobal  string `json:"viaGlobal,omitempty"`
	ViaProject string `json:"viaProject,omitempty"`
	ViaSession string `json:"viaSession,omitempty"`
	ViaFrom    string `json:"viaFrom,omitempty"`
	Detail     string `json:"detail,omitempty"`
	// Slash is the command that switches it ("raglit" for /raglit), for a
	// server that has one.
	Slash string `json:"slash,omitempty"`
	// Starting is a start in flight (the session is ready without it; its
	// tools join when it is up), StartingSeconds how long it has been at it.
	Starting        bool    `json:"starting,omitempty"`
	StartingSeconds float64 `json:"startingSeconds,omitempty"`
	// Index is the index a running server builds after its handshake
	// (poly-lsp-mcp): "building", "ready" or "failed"; "" for one that
	// builds none or says nothing of it. IndexSeconds is how long it has
	// been building, or how long the build took; IndexErr why it failed;
	// Exclude the directories left out of it, for this project.
	Index        string   `json:"index,omitempty"`
	IndexSeconds float64  `json:"indexSeconds,omitempty"`
	IndexErr     string   `json:"indexErr,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	// What /mcp's detail says of it: its schemas' cost in every request
	// (Cost), what they would cost were it not scripted (Standalone), dun's
	// line for it (Description) and what it says of itself while it runs
	// (Title, Version, Instructions).
	// Hosted: a scripted server the running shell connected.
	Hosted       bool   `json:"hosted,omitempty"`
	Cost         int    `json:"cost,omitempty"`
	Standalone   int    `json:"standalone,omitempty"`
	Description  string `json:"description,omitempty"`
	Title        string `json:"title,omitempty"`
	Version      string `json:"version,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// Log is the newest of its activity log, oldest first: dun's steps with
	// it and the lines it wrote on stderr.
	Log []ServerLogLine `json:"log,omitempty"`
}

// ServerLogLine is one line of a server's activity log. At is unix
// milliseconds.
type ServerLogLine struct {
	At     int64  `json:"at"`
	Text   string `json:"text"`
	Stderr bool   `json:"stderr,omitempty"`
}

// ServerEvent reports a server command's outcome and the new server set.
type ServerEvent struct {
	ID      string        `json:"id"`
	Action  string        `json:"action"`
	Message string        `json:"message"`
	Servers []ServerState `json:"servers"`
	Tools   []string      `json:"tools"`
	// Background: no command asked for this — an autostart server that came
	// up (or did not) after the session was ready.
	Background bool `json:"background,omitempty"`
}

// ControlEvent reports a control command's outcome.
type ControlEvent struct {
	ID      string `json:"id"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message"`
}

// Catalog is the skills and MCP prompts in reach.
type Catalog struct {
	Skills  []Skill  `json:"skills"`
	Prompts []Prompt `json:"prompts"`
}

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	UserOnly    bool   `json:"userOnly,omitempty"`
}

type Prompt struct {
	Server      string           `json:"server"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptText is a rendered MCP prompt, for the client to send as the user.
type PromptText struct {
	Text string `json:"text"`
}

// ResetEvent: the conversation was cleared.
type ResetEvent struct{}

// Agents is every sub-agent, the whole list each time.
type Agents struct {
	Agents []AgentInfo `json:"agents"`
}

type AgentInfo struct {
	ID      int    `json:"id"`
	Model   string `json:"model"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Prompt  string `json:"prompt"`
	Tokens  int    `json:"tokens"`
	Seconds int    `json:"seconds"`
}

// Jobs is every background job, the whole list each time. Times are unix
// seconds.
type Jobs struct {
	Jobs []JobInfo `json:"jobs"`
}

type JobInfo struct {
	ID      int    `json:"id"`
	Command string `json:"command"`
	Log     string `json:"log"`
	State   string `json:"state"`
	Bytes   int64  `json:"bytes"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended"`
	Code    int    `json:"code"`
	Muted   bool   `json:"muted"`
}

// AgentHistory is a sub-agent's conversation, answering an agent view.
type AgentHistory struct {
	Agent  int           `json:"agent"`
	Prompt string        `json:"prompt"`
	Items  []HistoryItem `json:"items"`
}

// AgentTold answers an agent tell.
type AgentTold struct {
	Agent   int    `json:"agent"`
	Message string `json:"message"`
}

type Suggestions struct {
	Items []Suggestion `json:"items"`
}

type Suggestion struct {
	Text string  `json:"text"`
	Prob float64 `json:"prob"`
}

// Exit: the engine is ending.
type Exit struct {
	Reason string `json:"reason"`
}

// --- what one client did, for every other ---

// UserEcho is a user message the engine took, sent to every client so each
// renders it — not just the one that typed it. Queued says it went into the
// running turn rather than starting one.
type UserEcho struct {
	Content string `json:"content"`
	Queued  bool   `json:"queued,omitempty"`
}

// ImageRefused: a user message with images was not sent, because the model
// cannot read images and no vision model is configured to describe them.
// Content and Images are the message as the client sent it, so the client
// can put it back in the composer; Message says why and what to configure.
type ImageRefused struct {
	Message string  `json:"message"`
	Content string  `json:"content"`
	Images  []Image `json:"images,omitempty"`
}

// Turn: a turn started. Kind is "user" (a message started it) or "continue"
// (background work did).
type Turn struct {
	Kind string `json:"kind"`
}

// Answered: the open ask (or mount) got its answer.
type Answered struct {
	Value string `json:"value"`
}

// Settings is the session's shared toggles, sent at ready and on each change.
type Settings struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	URL      string `json:"url"`
	Rephrase bool   `json:"rephrase"`
	Suggest  string `json:"suggest"` // on | off | auto
	// Context is /context config's settings in force.
	Context *ContextSettings `json:"context,omitempty"`
	// Effort is /effort's level.
	Effort string `json:"effort,omitempty"`
}

// ContextSettings is /context config's settings in force: the window (0
// unknown) and where it came from, the reply's buffer, the tool-result stub
// length and the recap reminder's size.
type ContextSettings struct {
	Max           int    `json:"max"`
	MaxFrom       string `json:"maxFrom,omitempty"`
	CompactBuffer int    `json:"compactBuffer"`
	ToolLOD       int    `json:"toolLod"`
	RecapNudge    int    `json:"recapNudge"`
}

// Commands is the slash commands a session takes, as data, for every client:
// a palette, argument completion, and toggles that show the current state.
// A client sends a line as the control command "slash" with the line (no
// leading /) as its action; the engine routes it. Skills and MCP prompts are
// in the catalog event, not here.
type Commands struct {
	Items []CommandSpec `json:"items"`
}

type CommandSpec struct {
	Name        string `json:"name"`
	Usage       string `json:"usage,omitempty"`
	Description string `json:"description"`
	// Group sorts commands for a panel: tools, git, memory, session.
	Group string `json:"group"`
	// Choices are the values the first argument takes.
	Choices []CommandChoice `json:"choices,omitempty"`
	// Text: free text follows (or is) the argument.
	Text bool `json:"text,omitempty"`
	// State is the current value for a toggle ("on", "off", a mode); empty
	// for an action.
	State string `json:"state,omitempty"`
	// Detail qualifies the state, e.g. "autostart".
	Detail string `json:"detail,omitempty"`
	// Help is the command in full, in plain words: what it does and how to
	// use it, for /help <name>. Empty: the description says it.
	Help string `json:"help,omitempty"`
}

type CommandChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// Models answers control "models": every saved provider's models, fetched
// by the engine (the keys never leave it). A client switches with dund's
// switchModel (the provider's URL and a model id).
type Models struct {
	Providers []ModelProvider `json:"providers"`
}

type ModelProvider struct {
	Name  string   `json:"name"`
	URL   string   `json:"url"`
	IDs   []string `json:"ids"`
	Error string   `json:"error,omitempty"`
	// Default marks the saved default provider; Key says whether one is
	// saved (the key itself stays on the machine dun runs on).
	Default bool `json:"default,omitempty"`
	Key     bool `json:"key,omitempty"`
}

// McpTools answers control "mcptools": every tool one server discovered,
// hidden ones included, with what each costs. Action "<id>" asks; "toggle
// <id> <tool> on|off" turns one on or off and answers with the new list.
// Message says what happened (the file written, or why not).
type McpTools struct {
	Server  string    `json:"server"`
	Tools   []McpTool `json:"tools"`
	Message string    `json:"message,omitempty"`
}

type McpTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      string `json:"schema,omitempty"` // the input schema, indented JSON
	Tokens      int    `json:"tokens"`
	Enabled     bool   `json:"enabled"`
	HiddenBy    string `json:"hiddenBy,omitempty"` // the Disable entry hiding it
	// The switch at each scope ("on", "off", "" unset) and the deciding one.
	Global  string `json:"global,omitempty"`
	Project string `json:"project,omitempty"`
	Session string `json:"session,omitempty"`
	From    string `json:"from,omitempty"`
}

// McpLogin asks the person to log in to an OAuth MCP server: open URL in a
// browser. The engine may run where no browser is (under dund), so this is
// the way the login reaches them. URL "" means the engine is waiting for a
// login another session started.
type McpLogin struct {
	Server string `json:"server"`
	URL    string `json:"url,omitempty"`
}

// --- the protocol's own ---

// Ack answers the command whose cid is the event's re. An empty Error is
// success.
type Ack struct {
	Error string `json:"error,omitempty"`
	// Reused: a create with ReuseIdle handed back an existing session.
	Reused bool `json:"reused,omitempty"`
}

// Snapshot replaces a replay the log can no longer give: a client that
// subscribed from before the oldest kept event, past the newest, or from
// another epoch (a seq numbered before the service restarted) gets the
// conversation so far and the latest state events, then the live stream
// from After+1. The snapshot event's epoch is the one After belongs to.
type Snapshot struct {
	After uint64        `json:"after"`
	Items []HistoryItem `json:"items"`
	State []Event       `json:"state"`
}

// Switches is a kind of scoped switches (skills; dun/switches.go): every item
// with its switch at each scope, the kind's mode at each scope and in force,
// what each mode means, and the answer to the command that asked (Message).
// A client asks with the control command "switches" and the kind as its
// action, optionally followed by "on|off|unset <name or glob> [--scope]" or
// "mode <mode|unset> [--scope]".
type Switches struct {
	Kind     string            `json:"kind"`
	Items    []SwitchItem      `json:"items"`
	Mode     string            `json:"mode"`
	ModeFrom string            `json:"modeFrom,omitempty"`
	Modes    map[string]string `json:"modes,omitempty"`
	About    map[string]string `json:"about,omitempty"`
	Order    []string          `json:"order,omitempty"` // the modes, the default first
	Note     string            `json:"note,omitempty"`
	Message  string            `json:"message,omitempty"`
	Slash    string            `json:"slash,omitempty"`  // the command: "skill", "rules"
	Status   []string          `json:"status,omitempty"` // facts for the status pane
}

// SkillFile answers the control command "skillfile", whose action is JSON
// {"skill": name, "file": path}: one file of a skill, for a person to read
// (the /skill browser). File "" is SKILL.md, whose front matter comes apart
// (Name, Description) from its body (Text). The path is confined to the
// skill's folder; Error says why a file was not read. Files is every file
// in the folder, to tell a link from a broken one; Includes is the text of
// each file an @path in it includes, to depth 3, by its path.
type SkillFile struct {
	Skill       string            `json:"skill"`
	File        string            `json:"file"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Text        string            `json:"text"`
	Clipped     bool              `json:"clipped,omitempty"`
	Error       string            `json:"error,omitempty"`
	Files       []string          `json:"files,omitempty"`
	Includes    map[string]string `json:"includes,omitempty"`
}

// SwitchItem is one item of a Switches event.
type SwitchItem struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Tokens      int    `json:"tokens"`
	On          bool   `json:"on"`
	From        string `json:"from,omitempty"`
	Global      string `json:"global,omitempty"`
	Project     string `json:"project,omitempty"`
	Session     string `json:"session,omitempty"`
}

// Extensions is every extension, for a UI's /ext list: the control command
// "ext" with the action "view" asks for it. Each one's server state is in
// the server events, by its id.
type Extensions struct {
	Items []ExtItem `json:"items"`
	// Errors are the manifests that did not load, and the parts refused.
	Errors []string `json:"errors,omitempty"`
	// AddPath is where a user's own extension goes.
	AddPath string `json:"addPath,omitempty"`
}

// ExtItem is one extension.
type ExtItem struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"` // bundled, user or project
	Path        string    `json:"path,omitempty"`
	Description string    `json:"description,omitempty"`
	Slash       string    `json:"slash,omitempty"` // its command, without the /
	Commands    []ExtVerb `json:"commands,omitempty"`
	Roles       []string  `json:"roles,omitempty"`     // "search:<tool>", "index:<tool>/<forget>"
	Hooks       []string  `json:"hooks,omitempty"`     // the dun hooks it subscribes to
	HostHooks   []string  `json:"hostHooks,omitempty"` // "<name>=<what>"
}

// ExtVerb is one of an extension's slash commands.
type ExtVerb struct {
	Name string `json:"name"`
	Help string `json:"help,omitempty"`
}
