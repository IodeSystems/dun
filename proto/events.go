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
	event[Turn]("turn")
	event[Answered]("answered")
	event[Settings]("settings")
	event[Commands]("commands")
	event[Models]("models")
	event[McpTools]("mcp_tools")
	event[McpLogin]("mcp_login")
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
}

// ServerEvent reports a server command's outcome and the new server set.
type ServerEvent struct {
	ID      string        `json:"id"`
	Action  string        `json:"action"`
	Message string        `json:"message"`
	Servers []ServerState `json:"servers"`
	Tools   []string      `json:"tools"`
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
	Blocked bool   `json:"blocked"`
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
