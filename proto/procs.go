package proto

import "time"

// Procs is what `dun ps` (dun dund sessions ps) asks dund: the caller's
// running sessions with what the processes on this machine are matched
// against — each engine's pid, the trees the session and its sub-agents
// work in, and its jobs' process groups. dund only names them; the client
// reads and samples the process table (internal/procps), as it runs on the
// same machine (USER 2026-11-17: "can't tell which dun session is
// responsible").
type Procs struct{}

// ProcsList answers Procs.
type ProcsList struct {
	DundPID  int           `json:"dund_pid"`
	Sessions []ProcSession `json:"sessions"`
	// Rag is retrieval inside dund (internal/ragd): its embed backend and
	// each open index's activity. nil: dund runs no retrieval, or is older.
	Rag *ProcRag `json:"rag,omitempty"`
}

// ProcRag is dund's retrieval activity, for dun ps.
type ProcRag struct {
	Embed   ProcRagEmbed   `json:"embed"`
	Indexes []ProcRagIndex `json:"indexes"`
}

// ProcRagEmbed is the embed backend. Model "" is no embed model (search by
// text only). Where is "local" (loopback or the local network) or
// "remote". Width, N429 and BackedOff are its admission channel's (raglit
// Channels): how many requests it admits at once, the 429s it has seen,
// and whether it is cooling down after one.
type ProcRagEmbed struct {
	Model     string `json:"model,omitempty"`
	Where     string `json:"where,omitempty"`
	InFlight  int    `json:"in_flight"`
	Width     int    `json:"width,omitempty"`
	N429      int    `json:"n_429,omitempty"`
	BackedOff bool   `json:"backed_off,omitempty"`
}

// ProcRagIndex is one open index. Name is its key ("<project>/<kind>",
// "global/<kind>", "<project>/branches/<worktree>/<kind>"); Project is ""
// for the global set; Branch is the worktree branch, "" the main checkout.
// State is idle, ingesting, embedding, backed-off or error. Queue is the
// pending jobs (QueueCapped: at least that many), Running the running ones.
// The *Min fields are the last minute; the others are totals since dund
// started. Files are the files jobs took and the texts dund ingested;
// chunks the inputs embedded (and each text); tokens are estimated as
// characters/4. IdleSec is the seconds since its last activity, -1 none.
type ProcRagIndex struct {
	Name          string    `json:"name"`
	Project       string    `json:"project"`
	Kind          string    `json:"kind"`
	Branch        string    `json:"branch,omitempty"`
	State         string    `json:"state"`
	Queue         int       `json:"queue"`
	QueueCapped   bool      `json:"queue_capped,omitempty"`
	Running       int       `json:"running"`
	EmbedInFlight int       `json:"embed_in_flight"`
	FilesMin      int64     `json:"files_min"`
	ChunksMin     int64     `json:"chunks_min"`
	EmbedsMin     int64     `json:"embeds_min"`
	EmbedMsMin    int64     `json:"embed_ms_min"`
	TokensMin     int64     `json:"tokens_min"`
	Files         int64     `json:"files"`
	Chunks        int64     `json:"chunks"`
	Embeds        int64     `json:"embeds"`
	EmbedMs       int64     `json:"embed_ms"`
	Tokens        int64     `json:"tokens"`
	LastErr       string    `json:"last_error,omitempty"`
	LastErrAt     time.Time `json:"last_error_at,omitzero"`
	LastActive    time.Time `json:"last_active,omitzero"`
	IdleSec       int64     `json:"idle_sec"`
}

// ProcSession is one running session.
type ProcSession struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	// Worktree is the tree the engine works in when it is not Workspace.
	Worktree string `json:"worktree,omitempty"`
	// Project is the workspace's rag project slug (harness.ProjectSlug):
	// the indexes in Rag of that project are the session's.
	Project   string `json:"project,omitempty"`
	EnginePID int    `json:"engine_pid"`
	// Blocked: EnginePID is an engine from before dund's restart that was
	// not adopted and may still run; the session starts no engine until it
	// is gone. Why it was not adopted.
	Blocked string `json:"blocked,omitempty"`
	// NotesWaiting: idle, with news that started no turn (no ask open, the
	// hour's automatic turns used): turn_state's notes_waiting.
	NotesWaiting int `json:"notes_waiting,omitempty"`
	// NetWaitSince: waiting for the network since then (Unix ms):
	// turn_state's net_wait.
	NetWaitSince int64       `json:"net_wait_since,omitempty"`
	Agents       []ProcAgent `json:"agents,omitempty"`
	Jobs         []ProcJob   `json:"jobs,omitempty"`
}

// ProcAgent is a live sub-agent that works in a tree of its own (Dir), runs
// on a model tier (Tier), or has an effective effort its engine said
// (Effort); any may be empty.
type ProcAgent struct {
	ID     int    `json:"id"`
	Dir    string `json:"dir"`
	Tier   string `json:"tier,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// ProcJob is a running background job.
type ProcJob struct {
	ID      int    `json:"id"`
	Command string `json:"command"`
	PGID    int    `json:"pgid,omitempty"`
}

func init() {
	command[Procs]("procs")
	event[ProcsList]("procs")
}
