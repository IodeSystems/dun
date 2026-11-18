package proto

// The engine link (docs/engine-adoption.md): dund reaches an engine it
// started with --listen over the engine's own unix socket instead of its
// stdio, so the engine outlives dund's restart and the next dund dials it
// back. These are the link's own frames; events and commands cross as
// before, an event prefixed by its link seq.

// LinkVersion changes only with an incompatible change of the link's wire:
// an engine of another link version is not adopted.
const LinkVersion = 1

// LinkHello is the engine's first line on every connection.
type LinkHello struct {
	Type    string `json:"type"` // "link_hello"
	Link    int    `json:"link"`
	Proto   int    `json:"proto"`
	Version string `json:"version,omitempty"` // the engine's build
	Pid     int    `json:"pid"`
	Session string `json:"session,omitempty"` // empty until the engine named it
	Seq     uint64 `json:"seq"`               // the last event written
	Acked   uint64 `json:"acked"`             // the last event a dund acked
	First   uint64 `json:"first"`             // the oldest event still kept
	// Shared is how many shared MCP servers the engine holds through dund's
	// gateway: they died with the dund that granted them.
	Shared int `json:"shared,omitempty"`
}

// LinkResume is dund's first line: send the events after After; with State,
// the latest state events at or before After first (an adoption).
type LinkResume struct {
	Type  string `json:"type"` // "link_resume"
	After uint64 `json:"after"`
	State bool   `json:"state,omitempty"`
	// Token is EngineInfo.Token: without it the engine closes the
	// connection, and the link stays free.
	Token string `json:"token"`
}

// LinkAck says dund took every event to Seq: the engine drops them.
type LinkAck struct {
	Type string `json:"type"` // "link_ack"
	Seq  uint64 `json:"seq"`
}

// LinkGap says the events after After and before First are gone: the ring
// overflowed while no dund was attached.
type LinkGap struct {
	Type  string `json:"type"` // "link_gap"
	After uint64 `json:"after"`
	First uint64 `json:"first"`
}

// Link frame types and the lines of those without fields.
const (
	LinkHelloType  = "link_hello"
	LinkResumeType = "link_resume"
	LinkAckType    = "link_ack"
	LinkGapType    = "link_gap"
	LinkOkLine     = `{"type":"link_ok"}`   // the resume is taken; the replay follows
	LinkLiveLine   = `{"type":"link_live"}` // the replay is over
	LinkBusyLine   = `{"type":"link_busy"}` // another dund is attached
)

// EngineInfo is <engines dir>/<pid>.json, the engine's entry in the
// directory dund scans at start.
type EngineInfo struct {
	Pid     int    `json:"pid"`
	Sock    string `json:"sock"`
	Session string `json:"session,omitempty"`
	Version string `json:"version,omitempty"`
	Proto   int    `json:"proto"`
	Link    int    `json:"link"`
	Started string `json:"started,omitempty"` // RFC 3339
	// Token authenticates a dialer (LinkResume.Token); the file is 0600.
	Token string `json:"token,omitempty"`
}

// IsStateType reports whether an event type is state: its latest event is
// the session's current state of that kind (Log.State).
func IsStateType(t string) bool { return stateTypes[t] }
