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
