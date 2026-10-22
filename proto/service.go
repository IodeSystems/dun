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
	command[Forget]("forget")
	command[Persist]("persist")
	command[Leave]("leave")
	command[Hello]("hello")
	command[Upgrade]("upgrade")

	event[SessionList]("sessions")
	event[EngineState]("engine")
	event[UIState]("ui_state")
	event[PersistState]("persist")
	event[HelloReply]("hello")
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

// Forget removes the envelope's session from the service: its engine is
// stopped and its registry entry and members' UI state are deleted. The
// transcript is the engine's to delete (the close control command).
type Forget struct{}

// Detach ends the caller's subscription to the envelope's session. The
// session keeps running.
type Detach struct{}

// Persist sets or reports whether the envelope's session keeps running when
// its last client leaves (and past the idle stop). Mode is "on", "off",
// "status", or "" — on if it is off, else status. Answered with a persist
// event.
type Persist struct {
	Mode string `json:"mode,omitempty"`
}

// PersistState is a session's persist setting; Changed says this command
// changed it.
type PersistState struct {
	On      bool `json:"on"`
	Changed bool `json:"changed,omitempty"`
}

// Hello is a client saying which build it is. dund answers with its own, so
// a client can tell that the service it reached is a different build.
type Hello struct {
	Version string `json:"version"`
	Exe     string `json:"exe,omitempty"`
}

// HelloReply is dund's build. Upgradable: it was started by a client, not a
// service manager, so an Upgrade will be honoured.
type HelloReply struct {
	Version    string `json:"version"`
	Exe        string `json:"exe,omitempty"`
	Upgradable bool   `json:"upgradable,omitempty"`
}

// Upgrade asks a client-started dund to restart into Exe (engines too), as
// soon as nothing is busy. The executable must be a regular file owned by
// dund's user. A dund run by a service manager refuses: the service is
// restarted instead.
type Upgrade struct {
	Exe string `json:"exe"`
}

// Leave is a client exiting (/exit): its subscription ends, and the engine
// stops unless the session persists or another client is attached. A
// session with no messages is then forgotten.
type Leave struct{}

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
	Persist   bool     `json:"persist,omitempty"`
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
