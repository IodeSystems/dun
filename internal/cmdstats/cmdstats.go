// Package cmdstats counts which slash commands get used: one JSON line per
// dispatch in $DUN_HOME/stats/commands.jsonl, the command's name and the
// surface that handled it, never its arguments. Local to the machine; nothing
// reads it back but the person who owns it.
package cmdstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/dunhome"
)

// The surfaces: the TUI handled the command itself, or the engine routed it
// (a line from the TUI, the web or the phone that reached routeSlash).
const (
	SurfaceTUI    = "tui"
	SurfaceEngine = "engine"
)

// Path is the counter's file.
func Path() string { return filepath.Join(dunhome.Dir(), "stats", "commands.jsonl") }

// nameOK keeps a line short and a name a name: what a mistyped line or an
// argument that slipped in would not look like.
var nameOK = regexp.MustCompile(`^/[a-z0-9][a-z0-9_-]{0,39}$`)

type line struct {
	TS      string `json:"ts"`
	Cmd     string `json:"cmd"`
	Surface string `json:"surface"`
}

// Record appends one line for the command cmd ("/context"; a leading slash is
// added). Best effort: a failure is dropped, the command goes on.
func Record(cmd, surface string) {
	cmd = "/" + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cmd)), "/")
	if !nameOK.MatchString(cmd) {
		return
	}
	b, err := json.Marshal(line{TS: time.Now().Format(time.RFC3339), Cmd: cmd, Surface: surface})
	if err != nil {
		return
	}
	p := Path()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n')) // one write: a short line stays whole across processes
}
