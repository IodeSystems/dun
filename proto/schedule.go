package proto

import "time"

// Scheduled messages and loops (/schedule, /loop, the loop_ctl tool): dund
// keeps the entries and fires them (internal/dund/schedule.go,
// docs/scheduling.md). A client's slash command reaches dund the way
// dun_msg does, through its engine, as a peer_request with Op "sched"; the
// answer is a peer_reply carrying Sched. dund tells an engine how many loops
// its session has with a loops command, which is what lists loop_ctl.

func init() {
	command[Loops]("loops")
}

// SelfPaced is an Every that means "a loop the model paces with loop_ctl".
const SelfPaced time.Duration = -1

// SchedReq is one scheduler operation, asked by an engine for its session.
//
// Action:
//   - "add": a message (Every 0) or a loop (Every > 0, or SelfPaced) of Text,
//     at At, to session To (a name or id; empty: the asking session).
//   - "list": every entry of the asking session's owner.
//   - "get": entry ID.
//   - "push": entry ID to At, or (Shift set) Shift past its current time.
//   - "edit": entry ID's text becomes Text.
//   - "cancel": entry ID goes.
//   - "loop_ctl": the asking session's last-fired loop: Stop it, or fire it
//     next NextIn from now. Reason is the model's, for the person.
type SchedReq struct {
	Action string        `json:"action"`
	ID     int           `json:"id,omitempty"`
	At     time.Time     `json:"at,omitzero"`
	Shift  time.Duration `json:"shift,omitempty"`
	Every  time.Duration `json:"every,omitempty"`
	To     string        `json:"to,omitempty"`
	Text   string        `json:"text,omitempty"`
	NextIn time.Duration `json:"next_in,omitempty"`
	Stop   bool          `json:"stop,omitempty"`
	Reason string        `json:"reason,omitempty"`
}

// SchedEntry is one pending message or loop, as a client lists it. Name is
// the target session's name (as dun_list names it), ByName the scheduling
// session's. At is zero while a self-paced loop waits for its turn to end
// (Awaiting).
type SchedEntry struct {
	ID        int           `json:"id"`
	Session   string        `json:"session"`
	Name      string        `json:"name,omitempty"`
	At        time.Time     `json:"at,omitzero"`
	Every     time.Duration `json:"every,omitempty"`
	Text      string        `json:"text"`
	By        string        `json:"by,omitempty"`
	ByName    string        `json:"by_name,omitempty"`
	Created   time.Time     `json:"created"`
	Iteration int           `json:"iteration,omitempty"`
	Awaiting  bool          `json:"awaiting,omitempty"`
}

// Loop reports whether the entry repeats.
func (e SchedEntry) Loop() bool { return e.Every != 0 }

// SchedReply answers a SchedReq: Entry is the one it made or changed (for
// cancel and a stop, as it was), Entries the list. Self is the asking
// session's id, and Loops how many loops it has after the operation.
type SchedReply struct {
	Entry   *SchedEntry  `json:"entry,omitempty"`
	Entries []SchedEntry `json:"entries,omitempty"`
	Self    string       `json:"self"`
	Loops   int          `json:"loops"`
}

// Loops tells an engine how many loops its session has: loop_ctl is in its
// tool set while Active > 0, and only then.
type Loops struct {
	Active int `json:"active"`
}
