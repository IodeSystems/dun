package proto

import "github.com/iodesystems/dun/internal/procstat"

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
	command[JobUsageReq]("job_usage")

	event[SessionList]("sessions")
	event[EngineState]("engine")
	event[UIState]("ui_state")
	event[PersistState]("persist")
	event[HelloReply]("hello")
	event[UpgradeStatus]("upgrade")
	event[JobUsage]("job_usage")
}

// JobUsageReq asks what background job Job's processes are doing: their
// state, CPU, memory, threads, open files, IO and where they wait. dund
// answers it for the session in the envelope (it samples the job's process
// group, from the session's jobs event), and so does an engine run without
// dund. The answer goes to the asker only: it is not in the session's log.
type JobUsageReq struct {
	Job int `json:"job"`
}

// JobUsage answers a JobUsageReq. Error is set when the job is not running
// or its processes cannot be read (a docker job). Verdict reads the sample:
// busy, waiting on IO, idle.
type JobUsage struct {
	Job     int            `json:"job"`
	Usage   procstat.Usage `json:"usage"`
	Verdict string         `json:"verdict,omitempty"`
	Error   string         `json:"error,omitempty"`
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
	// ReuseIdle (with no Resume) answers with the caller's most recent
	// session in Workspace that no client is attached to, created with the
	// same Args, and creates one only when there is none. The ack's Reused
	// says which. The pick is dund's, so two clients starting at once never
	// get the same session.
	ReuseIdle bool `json:"reuse_idle,omitempty"`
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

// HelloReply is dund's build. Upgradable: an Upgrade will be honoured — a
// dund a client started re-execs into the new binary; one a service manager
// runs copies it over the binary the service starts, and exits for the
// manager to restart it. NoUpgrade is why a service's dund cannot ("" when it
// can, or when an older dund would not say).
type HelloReply struct {
	Version    string `json:"version"`
	Exe        string `json:"exe,omitempty"`
	Upgradable bool   `json:"upgradable,omitempty"`
	NoUpgrade  string `json:"no_upgrade,omitempty"`
	// Busy: a turn, a question, a job or a sub-agent is running somewhere, so
	// an upgrade waits for it.
	Busy bool `json:"busy,omitempty"`
	// Coordinated: this dund broadcasts its upgrades (UpgradeStatus) and
	// resumes the work a restart cuts short. An older dund leaves it unset.
	Coordinated bool `json:"coordinated,omitempty"`
	// Upgrade is the pending upgrade, if there is one.
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`
}

// Upgrade asks dund to restart into Exe (engines too), as soon as nothing is
// busy. The executable must be a regular file owned by dund's user. A dund
// run by a service manager copies Exe over the binary its service starts,
// then exits for the manager to restart it; one that cannot (HelloReply's
// NoUpgrade) refuses.
type Upgrade struct {
	Exe string `json:"exe"`
	// Version is the build Exe is, when the client knows it (its own).
	Version string `json:"version,omitempty"`
	// Now: restart into the pending upgrade at once (Exe is then optional),
	// interrupting what it waits for; those sessions resume afterwards.
	Now bool `json:"now,omitempty"`
}

// UpgradeStatus is dund's restart into a new build, sent to every
// connection, whatever it follows, with no session in the envelope: when the
// upgrade becomes pending (a new binary is in place, or a client asked), each
// time what it waits for changes, and once more as "restarting" just before
// dund restarts. A connection made while one is pending gets it at once; a
// session stream over HTTP carries it too.
//
// State: "pending" (waiting for Waiting to end), "restarting", or "failed":
// the new binary could not be put in place (Error says why), nothing was
// replaced, and dund runs on with no upgrade pending. Version is the target
// build, "" when dund could not tell.
type UpgradeStatus struct {
	State   string        `json:"state"`
	Version string        `json:"version,omitempty"`
	Exe     string        `json:"exe,omitempty"`
	Waiting []UpgradeWait `json:"waiting,omitempty"`
	// Deadline (unix seconds) is when dund restarts anyway, interrupting
	// what it still waits for; those sessions resume after the restart.
	Deadline int64  `json:"deadline,omitempty"`
	Error    string `json:"error,omitempty"`
}

// UpgradeWait is one thing a pending upgrade waits for. What: "turn",
// "question", "job" (ID is the job's, Detail its command) or "agent" (ID is
// the sub-agent's, Detail its prompt). Hint is how to end it, in words a
// client may replace with its own keys.
type UpgradeWait struct {
	Session string `json:"session"`
	What    string `json:"what"`
	ID      int    `json:"id,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
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
	// Attached is how many clients follow the session right now.
	Attached int `json:"attached"`
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
