package proto

// dun↔dun messaging (dun_list, dun_msg): one session's model sees and
// messages the other live sessions of the same owner on the same dund. The
// engine has one channel to dund, its stdio, so the request rides it:
//
//	engine A ──peer_request──▶ dund ──peer──▶ engine B
//	engine A ◀──peer_reply──── dund
//
// dund answers for the requester's owner only, and never starts an engine
// to deliver: a session that is not loaded is not messageable.

func init() {
	event[PeerRequest]("peer_request")
	command[PeerReply]("peer_reply")
	command[PeerMsg]("peer")
	command[ServersChanged]("servers")
	command[OAuthCmd]("oauth")
	event[OAuthResult]("oauth_result")
}

// PeerRequest is an engine asking dund to list the live sessions (Op
// "list"), to deliver Text to session To (Op "msg"), or to run a scheduler
// operation (Op "sched", schedule.go). RID pairs it with
// its PeerReply. dund does not log it: it is plumbing, and the tool call
// that made it is in the log already.
type PeerRequest struct {
	RID  string `json:"rid"`
	Op   string `json:"op"`
	To   string `json:"to,omitempty"`
	Text string `json:"text,omitempty"`
	Wake bool   `json:"wake,omitempty"`
	// Sched is the operation for Op "sched" (schedule.go).
	Sched *SchedReq `json:"sched,omitempty"`
	// OAuth is the operation for Op "oauth": dund holds every OAuth
	// server's token and is the one process that refreshes it or logs in
	// (dun/oauthbroker.go).
	OAuth *OAuthReq `json:"oauth,omitempty"`
}

// PeerReply answers a PeerRequest. Error is set when it failed. For "list",
// Sessions; for "msg", ID and Name are the target's, and Busy and Started
// are what dund saw at delivery: the target was in a turn (it reads the
// message in it), or idle and woken (a turn starts there).
type PeerReply struct {
	RID      string        `json:"rid"`
	Error    string        `json:"error,omitempty"`
	Sessions []PeerSession `json:"sessions,omitempty"`
	ID       string        `json:"id,omitempty"`
	Name     string        `json:"name,omitempty"`
	Busy     bool          `json:"busy,omitempty"`
	Started  bool          `json:"started,omitempty"`
	Sched    *SchedReply   `json:"sched,omitempty"`
	OAuth    *OAuthResult  `json:"oauth,omitempty"`
}

// OAuthReq asks dund for an OAuth server's token (Op "token"), for one
// after a login if it has none ("ensure"), for a new one because the
// server refused the last ("unauthorized"), or to log in, log out or say
// the login's state ("login", "logout", "status"). Spec is the server's
// oauth block as dun.OAuthSpec, its client secret resolved. A login's URL
// goes to the asking session as an mcp_login event.
type OAuthReq struct {
	Op  string `json:"op"`
	ID  string `json:"id"`
	URL string `json:"url"`
	WWW string `json:"www,omitempty"` // the 401's WWW-Authenticate
	// Rejected is, for "unauthorized", the Authorization the server
	// refused; dund refreshes only when it is still dund's current one.
	Rejected string `json:"rejected,omitempty"`
	Spec     string `json:"spec"` // JSON
}

// OAuthResult answers an OAuthReq: the Authorization header value ("" for
// none), the login's state as dun.OAuthStatus, and for "logout" whether a
// token was revoked.
type OAuthResult struct {
	Authorization string `json:"authorization,omitempty"`
	Status        string `json:"status,omitempty"` // JSON
	Revoked       bool   `json:"revoked,omitempty"`
	Error         string `json:"error,omitempty"`
}

// OAuthCmd is OAuthReq from a client of dund's socket: `dun oauth token`,
// which mcpshell runs for a scripted OAuth server's bearer. Session, when
// set, is where a login's URL is shown.
type OAuthCmd struct {
	OAuthReq
}

// PeerSession is one live session, as dun_list shows it. Name is its
// address beside the id: "<folder>:<hhmmss>" — the workspace's folder (with
// parent folders when two listed workspaces share one) and the time part of
// the id — or the id itself when that is still ambiguous. State is "idle" or
// "busy"; Task is the session's status or goal, else its last user message,
// clipped. You marks the requester's own session.
type PeerSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Branch    string `json:"branch,omitempty"`
	// Worktree is the dun worktree the session works in, on Branch; empty
	// when it works in place. Name is always the source workspace's.
	Worktree string `json:"worktree,omitempty"`
	State    string `json:"state"`
	Attached int    `json:"attached"`
	Task     string `json:"task,omitempty"`
	You      bool   `json:"you,omitempty"`
}

// PeerMsg is a message from another session, delivered by dund. Wake: start
// a turn if the session is idle; otherwise it waits for the next turn.
type PeerMsg struct {
	From     string `json:"from"`
	FromName string `json:"from_name"`
	Text     string `json:"text"`
	Wake     bool   `json:"wake,omitempty"`
}

// ServersChanged tells an engine that another session of its owner wrote the
// global server file (~/.dun/mcp.json): an on/off, a mode, a tool switch, a
// server added or removed. The engine sends a PeerRequest with Op "servers"
// after the write; dund forwards this to the owner's other loaded sessions. An
// idle one re-reads its servers now; a busy one does at its next turn, as it
// would anyway (USER 2026-10-28: a global mode change reaches every session).
type ServersChanged struct{}
