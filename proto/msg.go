package proto

// dun msg (cmd/dun/msgcmd.go): messages between dun sessions and anything
// outside them — a shell, a script, another agent — through dund (USER
// 2026-11-04). A listener is a name a client holds while its connection
// lasts: dund gives it a token, streams it what is sent to the name, and
// forgets both when the connection closes. Sessions see listeners in dun_list
// and reach them with dun_msg.

func init() {
	command[MsgListen]("msg_listen")
	command[MsgWho]("msg_who")
	command[MsgSend]("msg_send")
	event[MsgToken]("msg_token")
	event[MsgIn]("msg_in")
	event[MsgWhoList]("msg_who")
	event[MsgSent]("msg_sent")
}

// MsgListen claims Name for this connection. Answered with a MsgToken, then
// a MsgIn for each message sent to the name, until the connection closes. A
// name another listener holds, or a live session's, is refused.
type MsgListen struct {
	Name string `json:"name"`
}

// MsgToken is a listener's: the token sends as its name (MsgSend.Token).
type MsgToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// MsgIn is one message to a listener. From is the sender's name: a
// session's (as dun_list names it), a listener's, or "cli".
type MsgIn struct {
	From string `json:"from"`
	Text string `json:"text"`
}

// MsgWho asks for everything a message can go to.
type MsgWho struct{}

// MsgWhoList answers MsgWho: the caller's live sessions, and the listeners.
type MsgWhoList struct {
	Sessions  []PeerSession `json:"sessions"`
	Listeners []string      `json:"listeners"`
}

// MsgSend sends Text to To (a session's name or id, or a listener's name),
// as the listener Token names, else as "cli". An idle session is woken unless
// NoWake.
type MsgSend struct {
	Token  string `json:"token,omitempty"`
	To     string `json:"to"`
	Text   string `json:"text"`
	NoWake bool   `json:"no_wake,omitempty"`
}

// MsgSent answers MsgSend: who it reached, and what happened there.
type MsgSent struct {
	To     string `json:"to"`
	Detail string `json:"detail"`
}
