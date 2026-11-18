package proto

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
}

// ProcSession is one running session.
type ProcSession struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	// Worktree is the tree the engine works in when it is not Workspace.
	Worktree  string      `json:"worktree,omitempty"`
	EnginePID int         `json:"engine_pid"`
	Agents    []ProcAgent `json:"agents,omitempty"`
	Jobs      []ProcJob   `json:"jobs,omitempty"`
}

// ProcAgent is a live sub-agent that works in a tree of its own.
type ProcAgent struct {
	ID  int    `json:"id"`
	Dir string `json:"dir"`
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
