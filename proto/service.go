package proto

// The service's own frames: what a client says to dund rather than to one
// session's engine, and what dund says back. A command naming a session in
// its envelope goes to that session; the ones here are about sessions.

func init() {
	command[ListSessions]("sessions")
	command[Create]("create")
	command[Detach]("detach")
	command[UIGet]("ui_get")
	command[UIPut]("ui_put")
	command[Restart]("restart")

	event[SessionList]("sessions")
	event[EngineState]("engine")
	event[UIState]("ui_state")
}

// ListSessions asks for the sessions the caller is a member of.
type ListSessions struct{}

// Create starts a session in a workspace. Args are engine flags from an
// allowlist (dund rejects the rest). The ack's envelope names the new
// session; the caller is its owner and only member.
//
// Resume adopts an existing conversation (a session file in the workspace)
// under that id; a session dund already has is answered with its id.
type Create struct {
	Workspace string   `json:"workspace"`
	Args      []string `json:"args,omitempty"`
	Resume    string   `json:"resume,omitempty"`
}

// Restart restarts the envelope's session's engine with new flags (same
// allowlist as create) on the same conversation: a model switch.
type Restart struct {
	Args []string `json:"args"`
}

// Detach ends the caller's subscription to the envelope's session. The
// session keeps running.
type Detach struct{}

// UIGet asks for the caller's UI state in the envelope's session.
type UIGet struct{}

// UIPut replaces the caller's UI state in the envelope's session. Its other
// connections to the session get it as a ui_state event; other members never
// see it.
type UIPut struct {
	State map[string]any `json:"state"`
}

// SessionList answers sessions.
type SessionList struct {
	Items []SessionSummary `json:"items"`
}

type SessionSummary struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	Owner     string   `json:"owner"`
	Members   []string `json:"members"`
	Running   bool     `json:"running"`
	LastSeq   uint64   `json:"last_seq"`
}

// EngineState is the session's engine process: started, exited, restarting
// (with how many restarts this window), gave_up, or stopped on request.
type EngineState struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

// UIState is one user's UI state for one session: opaque to dund, owned by
// the clients.
type UIState struct {
	State map[string]any `json:"state"`
}
