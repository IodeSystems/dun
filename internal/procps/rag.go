package procps

import (
	"fmt"
	"time"

	"github.com/iodesystems/dun/proto"
)

// Retrieval inside dund (internal/ragd) runs in the dund process: the
// table shows it as dund's CPU only. dund answers its indexes' activity
// with its sessions (proto.ProcsList.Rag), and dun ps puts one line per
// active index under dund's row, attributed to the session(s) whose
// workspace is the index's project (USER 2026-11-17).

// RagIdleAfter: an index idle this long (and with nothing queued) folds
// into the "N idle indexes" line.
const RagIdleAfter = 600 // seconds

// GroupGlobal owns the global indexes (memories and strategies of no
// project).
const GroupGlobal = "(global)"

// RagIndex is one index, attributed: Group and Session are the session
// whose workspace's project it is (the first, when several are; Others
// counts the rest), GroupGlobal for a global index, else GroupNone.
type RagIndex struct {
	proto.ProcRagIndex
	Group   string `json:"group"`
	Session string `json:"session"`
	Others  int    `json:"other_sessions,omitempty"`
	// ErrAgeSec is the age of LastErr at the reading; -1 none.
	ErrAgeSec int64 `json:"error_age_sec"`
}

// RaglitIndexes is the hook for the raglit daemon's indexes under its row
// (roleRaglitDaemon), from `raglit status --json --all`, which raglit does
// not have yet (asked of raglit's owner, 2026-11-17). nil: not read. When
// it exists, set it to run that command (as ReadRoots runs poly-lsp-mcp's
// status) and convert its indexes; dundcli's sample then calls AttachRag
// on the daemon's pid with them, and the rows render as dund's do.
var RaglitIndexes func(d *Proc) (*proto.ProcRag, error)

// RaglitDaemon is the raglit daemon in snap, nil if none.
func RaglitDaemon(snap Snapshot) *Proc {
	for _, pid := range sortedPIDs(snap) {
		if p := snap.Procs[pid]; Role(p.Argv) == roleRaglitDaemon {
			return p
		}
	}
	return nil
}

// AttachRag gives the row of pid (dund's, or the raglit daemon's) the
// indexes of rag, each attributed by sessions' projects: the active ones in
// Rag, the ones idle RagIdleAfter in RagIdle.
func AttachRag(rows []Row, pid int, rag *proto.ProcRag, sessions []proto.ProcSession, now time.Time) {
	if rag == nil || pid == 0 {
		return
	}
	for i := range rows {
		if rows[i].PID != pid {
			continue
		}
		e := rag.Embed
		rows[i].RagEmbed = &e
		for _, ix := range rag.Indexes {
			r := RagIndex{ProcRagIndex: ix, Group: GroupNone, Session: GroupNone, ErrAgeSec: -1}
			if !ix.LastErrAt.IsZero() {
				r.ErrAgeSec = max(0, int64(now.Sub(ix.LastErrAt).Seconds()))
			}
			if ix.Project == "" {
				r.Group, r.Session = GroupGlobal, GroupGlobal
			} else {
				for _, s := range sessions {
					if s.Project != ix.Project {
						continue
					}
					if r.Group == GroupNone {
						r.Group, r.Session = s.ID, SessionLabel(s.ID, s.Workspace)
					} else {
						r.Others++
					}
				}
			}
			if RagActive(ix) {
				rows[i].Rag = append(rows[i].Rag, r)
			} else {
				rows[i].RagIdle = append(rows[i].RagIdle, r)
			}
		}
		return
	}
}

// RagActive: the index is not idle, or was active in the last RagIdleAfter.
func RagActive(ix proto.ProcRagIndex) bool {
	return ix.State != "idle" || ix.Queue > 0 || ix.Running > 0 || (ix.IdleSec >= 0 && ix.IdleSec < RagIdleAfter)
}

// RagSession is the SESSION column of an index's line.
func RagSession(r RagIndex) string {
	if r.Others > 0 {
		return fmt.Sprintf("%s +%d", r.Session, r.Others)
	}
	return r.Session
}

// RagLine is what the JOB column says of an index:
// "rag <project> <kind>  <state>  queue N  files/min F  embeds/min E  err <age> <error>".
func RagLine(r RagIndex) string {
	project := r.Project
	if project == "" {
		project = "global"
	}
	kind := r.Kind
	if r.Branch != "" {
		kind += "@" + r.Branch
	}
	queue := fmt.Sprint(r.Queue)
	if r.QueueCapped {
		queue += "+"
	}
	s := fmt.Sprintf("rag %s %s  %s  queue %s  files/min %d  embeds/min %d", project, kind, r.State, queue, r.FilesMin, r.EmbedsMin)
	if r.LastErr != "" {
		s += "  err " + Age(r.ErrAgeSec) + " " + oneLine(r.LastErr)
	}
	return s
}

// RagEmbedLine is the embed backend's line: its model and where it runs,
// the requests in flight, its admission width, and a backoff.
func RagEmbedLine(e proto.ProcRagEmbed) string {
	if e.Model == "" {
		return fmt.Sprintf("rag embed off (search by text only)  in flight %d", e.InFlight)
	}
	s := "rag embed " + e.Model
	if e.Where != "" {
		s += " (" + e.Where + ")"
	}
	s += fmt.Sprintf("  in flight %d", e.InFlight)
	if e.Width > 0 {
		s += fmt.Sprintf("  width %d", e.Width)
	}
	if e.N429 > 0 {
		s += fmt.Sprintf("  429s %d", e.N429)
	}
	if e.BackedOff {
		s += "  backed off"
	}
	return s
}

// RagIdleLine is the line the idle indexes fold into.
func RagIdleLine(n int) string {
	if n == 1 {
		return "1 idle index"
	}
	return fmt.Sprintf("%d idle indexes", n)
}

// RagLines are the lines under a row (session, job), in the table's order:
// the embed backend when an index is active or a request is in flight,
// each active index, then the idle ones folded into one line.
func RagLines(r Row) [][2]string {
	if r.RagEmbed == nil {
		return nil
	}
	var out [][2]string
	if len(r.Rag) > 0 || r.RagEmbed.InFlight > 0 {
		out = append(out, [2]string{"", "  " + RagEmbedLine(*r.RagEmbed)})
	}
	for _, ix := range r.Rag {
		out = append(out, [2]string{RagSession(ix), "  " + RagLine(ix)})
	}
	if n := len(r.RagIdle); n > 0 {
		out = append(out, [2]string{"", "  " + RagIdleLine(n)})
	}
	return out
}
