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

// Stop ends the engine without waiting for background jobs.
type Stop struct{}

// Subscribe asks for every event after seq After (0: from the start), then
// the live stream. See Log.Subscribe.
type Subscribe struct {
	After uint64 `json:"after"`
}
