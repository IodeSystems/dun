package main

import (
	"os"
	"path/filepath"
)

// The launcher protocol — line-delimited JSON over a unix socket
// ($DUN_HOME/launcher.sock). One long-lived REGISTER connection per session
// (its close = the session left); short request/response connections for
// status / shutdown / reload. See launcher.go (daemon) + launcherclient.go
// (clients).

func launcherSocket() string { return filepath.Join(dunHome(), "launcher.sock") }

// req is a client→launcher message. Op selects the handler.
type req struct {
	Op        string `json:"op"`                  // "register" | "status" | "shutdown" | "reload"
	Kind      string `json:"kind,omitempty"`      // register: tui | web | task
	PID       int    `json:"pid,omitempty"`       // register: the session's process id
	Workspace string `json:"workspace,omitempty"` // register: the session's workspace
	Version   string `json:"version,omitempty"`   // register: the session's built version
	Force     bool   `json:"force,omitempty"`     // shutdown: proceed even with sessions attached
}

// resp is a launcher→client message.
type resp struct {
	OK       bool       `json:"ok"`
	ID       string     `json:"id,omitempty"`       // register: assigned session id
	Version  string     `json:"version,omitempty"`  // the launcher's current built version
	Sessions []sessMeta `json:"sessions,omitempty"` // status
	Attached int        `json:"attached,omitempty"` // shutdown: sessions still attached
	Web      int        `json:"web,omitempty"`      // shutdown: web sessions among them
	Err      string     `json:"err,omitempty"`
}

// push is an unsolicited launcher→session message on the REGISTER connection.
type push struct {
	Event   string `json:"event"`             // "reload"
	Version string `json:"version,omitempty"` // the new build's version
}

// sessMeta is one registered session, for `dun -d status`.
type sessMeta struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
	Workspace string `json:"workspace"`
	Version   string `json:"version"`
	AgeSec    int    `json:"age_sec"`
}

// selfKind labels this process for registration.
func selfKind(prog bool) string {
	if os.Getenv("DUN_CHILD") != "" || prog {
		return "web" // a spawned -tui (web PTY) or engine
	}
	return "tui"
}
