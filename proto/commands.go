package proto

// Command payloads. As with events, the names are the ones the engine reads
// today (cmd/dun main.go, inputStream).

func init() {
	command[User]("user")
	command[Answer]("answer")
	command[ServerCmd]("server")
	command[ControlCmd]("control")
	command[AgentCmd]("agent")
	command[Reset]("reset")
	command[Stop]("stop") // "quit" is accepted as an alias
	command[Interrupt]("interrupt")
	command[Subscribe]("subscribe")
}

// User is a message from the human. Mid-turn, it is queued (see Queued).
type User struct {
	Content string `json:"content"`
}

// Answer replies to the open ask or mount.
type Answer struct {
	Value string `json:"value"`
}

// ServerCmd turns a tool server on, off, auto or manual (/rag, /lsp).
type ServerCmd struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// ControlCmd is a slash command the engine runs: /docker, /worktree, /mcp, …
type ControlCmd struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// AgentCmd views or steers a sub-agent. ID is its number, as a string.
type AgentCmd struct {
	ID      string `json:"id"`
	Action  string `json:"action"` // view | tell
	Content string `json:"content,omitempty"`
}

// Reset clears the conversation.
type Reset struct{}

// Interrupt stops the turn in flight and keeps the session. What the turn
// had produced stays in the transcript, with a marker the model reads; the
// turn ends with done (reason "interrupted"). Messages queued during it start
// the next turn at once. With no turn running it is answered by a control
// event that says so.
type Interrupt struct{}

// Stop ends the engine without waiting for background jobs.
type Stop struct{}

// Subscribe asks for every event after seq After (0: from the start), then
// the live stream. See Log.Subscribe. Epoch is the epoch After was numbered
// in (empty: this log's). After a service restart it is not the log's, and
// the answer starts with a snapshot.
type Subscribe struct {
	After uint64 `json:"after"`
	Epoch string `json:"epoch,omitempty"`
}
