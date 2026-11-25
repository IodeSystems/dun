// Package sesslist is the one fast session listing: `dun sessions` and the
// TUI's /resume picker both build their rows with Collect.
//
// dund's session list says whether a session's engine runs, not what it is
// doing, so a status is running, stopped (dund knows it, no engine) or saved
// (a file only; every session when dund is down). The last request is read
// from the end of the transcript, and only for the rows returned: the store
// is tens of megabytes.
package sesslist

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/project"
	"github.com/iodesystems/dun/proto"
)

// Row is one session as `dun sessions --json` prints it.
type Row struct {
	ID         string    `json:"id"`
	Workspace  string    `json:"workspace"`
	Local      bool      `json:"local"`  // in the asked workspace's session directory
	Status     string    `json:"status"` // running, stopped, saved
	LastActive time.Time `json:"last_active"`
	Last       string    `json:"last_request,omitempty"`
	Attached   int       `json:"attached,omitempty"`
}

// Opts says what to list.
type Opts struct {
	WS  string // the workspace, absolute
	All bool   // every workspace, not just WS
	N   int    // rows; 0 = all
	// Dund lists dund's sessions; an error means dund is down.
	Dund func() ([]proto.SessionSummary, error)
}

// Collect merges dund's list with the session files. Rows of WS come first,
// then the others; each group newest first. total counts every session
// found, the ones past N unchecked.
func Collect(o Opts) (rows []Row, total int, dundUp bool) {
	// A session is keyed by its directory on disk: a linked worktree and its
	// main checkout share one. harness.RootDir finds it by asking the VCS, so
	// it runs once per workspace; the registry's entries need none.
	dirs := map[string]string{} // workspace -> session directory
	dirOf := func(ws string) string {
		if d, ok := dirs[ws]; ok {
			return d
		}
		d := harness.RootDir(ws)
		dirs[ws] = d
		return d
	}
	local := dirOf(o.WS)
	type key struct{ dir, id string }
	known := map[key]*Row{}
	asked := map[key]bool{}       // dund lists asked-in sessions: no file to open
	var scan []string             // session directories to list
	scanWS := map[string]string{} // directory -> the workspace shown
	add := func(ws, dir string) {
		if _, ok := scanWS[dir]; !ok {
			scanWS[dir] = ws
			scan = append(scan, dir)
		}
	}
	add(o.WS, local)
	if o.All {
		if es, err := project.List(); err == nil {
			for _, e := range es {
				if _, ok := dirs[e.Source]; !ok {
					dirs[e.Source] = project.SessionDir(e.ID)
				}
				add(e.Source, dirs[e.Source])
			}
		}
	}
	list, err := o.Dund()
	dundUp = err == nil
	for _, s := range list {
		d := dirOf(s.Workspace)
		if !o.All && d != local {
			continue
		}
		k := key{d, s.ID}
		r := &Row{ID: s.ID, Workspace: s.Workspace, Status: "stopped", Attached: s.Attached}
		if s.Running {
			r.Status = "running"
		}
		if t, err := time.Parse(time.RFC3339, s.LastActive); err == nil {
			r.LastActive = t
		}
		known[k], asked[k] = r, true
		add(s.Workspace, d)
	}
	for _, dir := range scan {
		for _, id := range harness.SessionIDsIn(dir) {
			if k := (key{dir, id}); known[k] == nil {
				known[k] = &Row{ID: id, Workspace: scanWS[dir], Status: "saved"}
			}
		}
	}
	order := make([]key, 0, len(known))
	for k, r := range known {
		r.Local = k.dir == local
		if r.LastActive.IsZero() {
			if st, err := os.Stat(filepath.Join(k.dir, k.id+".jsonl")); err == nil {
				r.LastActive = st.ModTime()
			}
		}
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := known[order[i]], known[order[j]]
		if a.Local != b.Local {
			return a.Local
		}
		if !a.LastActive.Equal(b.LastActive) {
			return a.LastActive.After(b.LastActive)
		}
		return a.ID > b.ID
	})
	// Only the rows returned are read: the tail gives the last request, and a
	// session with none there is opened whole (SessionAskedIn) to tell whether
	// it was ever asked in. The rest are counted, unchecked.
	for i, k := range order {
		if o.N > 0 && len(rows) >= o.N {
			return rows, len(rows) + len(order) - i, dundUp
		}
		r := known[k]
		r.Last = harness.LastAsk(k.dir, k.id)
		if r.Last == "" && !asked[k] && !harness.SessionAskedIn(k.dir, k.id) {
			continue // never asked in: not worth listing
		}
		rows = append(rows, *r)
	}
	return rows, len(rows), dundUp
}

// TildeDir is dir with $HOME as ~.
func TildeDir(dir string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if dir == h {
			return "~"
		}
		if rest, ok := strings.CutPrefix(dir, h+string(filepath.Separator)); ok {
			return "~/" + rest
		}
	}
	return dir
}

// ClipLeft keeps the tail of s within n runes: a path's end says more.
func ClipLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

// Age is the time since t as "3m ago"; "-" when unknown.
func Age(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d ago"
}
