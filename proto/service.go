package proto

import (
	"sort"
	"time"

	"github.com/iodesystems/dun/internal/procstat"
)

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
	command[MoveEngine]("move_engine")
	command[Forget]("forget")
	command[Persist]("persist")
	command[Leave]("leave")
	command[Hello]("hello")
	command[Upgrade]("upgrade")
	command[JobUsageReq]("job_usage")
	command[ServiceNotice]("service_notice")
	command[UpgradeHold]("upgrade_hold")
	command[WebSet]("web_set")

	event[SessionList]("sessions")
	event[EngineState]("engine")
	event[UIState]("ui_state")
	event[PersistState]("persist")
	event[HelloReply]("hello")
	event[UpgradeStatus]("upgrade")
	event[JobUsage]("job_usage")
	event[WebState]("web")
	event[WakeHeld]("wake_held")
}

// UpgradeHold tells an engine that an upgrade is pending (On) or no longer
// is (USER 2026-11-17). While held, the engine starts no turn of its own
// accord — a finished job, a peer's dun_msg, a server's wake, an aside
// that escalates: everything that goes through its wake. It writes that
// news into the conversation instead (persisted, so the restart keeps it)
// and says so with WakeHeld; dund then wakes the session after the
// restart. A person's message still starts a turn, and so does a
// ServiceNotice that is not quiet (dund's one wake while pending). Off,
// what was held runs at once. dund sends it to every engine when an
// upgrade becomes pending, to each engine that starts while one is, and
// Off when the upgrade fails.
//
// Gen orders them: dund counts up at each change, and an engine ignores a
// hold older than one it has (an engine that starts as an upgrade fails
// may get the failure's Off before its own On).
type UpgradeHold struct {
	On  bool  `json:"on"`
	Gen int64 `json:"gen,omitempty"`
}

// WakeHeld is an engine saying that, held (UpgradeHold), it kept Count
// items of news that would have started a turn: they are in the
// conversation now, and the next turn reads them.
type WakeHeld struct {
	Count int `json:"count"`
}

// WebSet changes dund's HTTP listener while it runs (`dun dund enable|disable
// web|tui`): Enable starts or stops it, Addr moves it (host:port; "" keeps
// the one it has), TUI turns /tui on or off. A nil field is left as it is, so
// a WebSet with none set asks for the state. The change is saved
// (<home>/dund/web.json) and applies at every later start. Answered with
// WebState, or an ack with the error (a port that cannot be bound: the
// listener stays as it was).
//
// TrustProxies, when not nil, replaces the proxies (CIDRs or addresses)
// whose X-Forwarded-Proto and -Host dund believes, besides loopback; an
// empty list clears them.
//
// PublicURL, when set, is the scheme://host a phone reaches dund at (a
// tunnel, a proxy); saved. Pair asks, after the change, for a one-time
// pairing code (dund pair.go): answered in WebState.Pair, refused while
// the listener is off or no public URL is saved.
type WebSet struct {
	Enable       *bool    `json:"enable,omitempty"`
	Addr         string   `json:"addr,omitempty"`
	TUI          *bool    `json:"tui,omitempty"`
	TrustProxies []string `json:"trust_proxies"` // null: unchanged; []: none
	PublicURL    string   `json:"public_url,omitempty"`
	Pair         bool     `json:"pair,omitempty"`
}

// WebPair is a one-time pairing code's link (<public_url>/auth/pair#code=...)
// and when it lapses.
type WebPair struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

// WebState is dund's HTTP listener after a WebSet. Listening: it is serving
// now. URL is where to open it; Warning is said to the person who made the
// change (the TUI on a non-loopback address).
type WebState struct {
	Enabled   bool   `json:"enabled"`
	Addr      string `json:"addr"`
	TUI       bool   `json:"tui"`
	Listening bool   `json:"listening,omitempty"`
	// TrustProxies are the trusted proxies besides loopback.
	TrustProxies []string `json:"trust_proxies,omitempty"`
	URL          string   `json:"url,omitempty"`
	Warning      string   `json:"warning,omitempty"`
	PublicURL    string   `json:"public_url,omitempty"`
	Pair         *WebPair `json:"pair,omitempty"`
}

// ServiceNotice is dund's word to a session's engine about the service
// itself (internal/dund/upgrade.go): an upgrade is coming and the session
// should wrap up its work, or the upgrade is done and it can resume. The
// engine hands it to the model, labelled as dun's, and wakes it if idle.
// Quiet: it wakes nothing — it reaches a running turn (in its next tool
// result, or by freeing a wait) or rides the next turn that starts for
// another reason; an idle session is not woken for it.
type ServiceNotice struct {
	Text  string `json:"text"`
	Quiet bool   `json:"quiet,omitempty"`
}

// JobUsageReq asks what background job Job's processes are doing: their
// state, CPU, memory, threads, open files, IO and where they wait. dund
// answers it for the session in the envelope (it samples the job's process
// group, from the session's jobs event). The answer goes to the asker only:
// it is not in the session's log. The model gets the same reading in its
// jobs replies (bgjob.go usage).
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

// MoveEngine restarts the envelope's session's engine, same flags, same
// conversation, on the build dund starts engines on, now: /upgrade now on
// a session whose adopted engine of an older build has not moved (a job or
// sub-agent kept it busy). A turn, job or sub-agent of it ends.
type MoveEngine struct{}

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
	// LastActive is the session's last activity, RFC3339 in UTC: the
	// transcript's modification time (it survives a dund restart), else the
	// moment dund last touched the session. "" when neither is known.
	LastActive string `json:"last_active,omitempty"`
}

// SessionBefore is the one session order: running first, then the most
// recently active, then the greater id. dund lists in it and the TUI's
// picker merges with it; clients show it and do not re-derive it.
func SessionBefore(a, b SessionSummary) bool {
	if a.Running != b.Running {
		return a.Running
	}
	// UTC RFC3339 compares as text; "" (unknown) sorts last.
	if a.LastActive != b.LastActive {
		return a.LastActive > b.LastActive
	}
	return a.ID > b.ID
}

// SortSessions orders items by SessionBefore.
func SortSessions(items []SessionSummary) {
	sort.SliceStable(items, func(i, j int) bool { return SessionBefore(items[i], items[j]) })
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
