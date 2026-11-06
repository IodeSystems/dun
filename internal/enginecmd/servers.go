package enginecmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/mcptable"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// SlashOf is the slash command of st's extension, or "" for a server with
// none. A state built without one (a test's) falls back to the bundled
// extension of its id.
func SlashOf(st dun.ServerState) string {
	if st.Slash != "" {
		return st.Slash
	}
	for _, e := range dun.BundledExtensions() {
		if e.ID == st.ID {
			return e.SlashName()
		}
	}
	return ""
}

// AllServers is the id /mcp uses to address every configured server at once.
// Not a legal server id (ids come from config and from dun.DefaultServers), so
// it cannot collide with one.
const AllServers = "*"

// LoginLine is the line that asks the person to log in to an OAuth server.
func LoginLine(server, url string) string {
	if url == "" {
		return server + ": waiting for a login to it that another session started"
	}
	return "open to log in to " + server + ": " + url
}

// ServerHint is the startup lines naming what did NOT start and why, plus the
// docs server's ingest summary when it autostarted. Shown once, at ready.
//
// The optional extras that are simply off (a server with a switch, /rag,
// /lsp) are not here: the client's splash and /status say them, with
// the command that turns each on (proto.ServerState.Slash).
func ServerHint(states []dun.ServerState) string {
	var lines []string
	for _, st := range states {
		if st.Running {
			// An autostarted docs server says what it indexed and skipped.
			if st.Ingest != "" {
				lines = append(lines, st.Ingest)
			}
			continue
		}
		if st.Via == dun.ViaShell {
			if st.Err != "" {
				lines = append(lines, fmt.Sprintf("%s (scripted) did not connect: %s", st.ID, textfmt.OneLine(st.Err)))
			}
			continue
		}
		switch {
		case st.Missing:
			lines = append(lines, fmt.Sprintf("%s off: %s", st.ID, textfmt.OneLine(st.Err)))
		case st.Err != "":
			lines = append(lines, fmt.Sprintf("%s did not start: %s", st.ID, textfmt.OneLine(st.Err)))
		case SlashOf(st) != "":
			// an optional extra, off: the splash says it
		default:
			lines = append(lines, st.ID+" off")
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ServersProto renders server states for an event.
func ServersProto(states []dun.ServerState) []proto.ServerState {
	out := make([]proto.ServerState, 0, len(states))
	for _, st := range states {
		out = append(out, proto.ServerState{ID: st.ID, Running: st.Running, Auto: st.Auto, Tools: st.Tools, Err: st.Err, Missing: st.Missing, Auth: st.Auth,
			Via: st.Via, Proto: st.Proto, Global: st.Global, Project: st.Project, Session: st.Session, From: st.From,
			ViaGlobal: st.ViaGlobal, ViaProject: st.ViaProject, ViaSession: st.ViaSession, ViaFrom: st.ViaFrom,
			Instance: st.Instance, InstanceGlobal: st.InstanceGlobal, InstanceProject: st.InstanceProject, InstanceSession: st.InstanceSession, InstanceFrom: st.InstanceFrom,
			Detail: mcptable.ServerDetail(st), Slash: SlashOf(st), Starting: st.Starting, StartingSeconds: st.StartingSeconds,
			Index: st.Index, IndexSeconds: st.IndexSeconds, IndexErr: st.IndexErr, Exclude: st.Exclude,
			Hosted: st.Hosted, Cost: st.Cost, Standalone: st.Standalone, Description: st.Description, Title: st.Title,
			Version: st.Version, Instructions: st.Instructions, Log: logProto(st.Log)})
	}
	return out
}

func logProto(lines []dun.ServerLogLine) []proto.ServerLogLine {
	out := make([]proto.ServerLogLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, proto.ServerLogLine{At: l.At.UnixMilli(), Text: l.Text, Stderr: l.Stderr})
	}
	return out
}
