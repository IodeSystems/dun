package procps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
	"github.com/iodesystems/dun/internal/textfmt"
)

// StatusTimeout bounds `poly-lsp-mcp status`: a hung daemon must not hang
// dun ps.
const StatusTimeout = 2 * time.Second

// Root is one root the shared poly-lsp-mcp daemon holds, its languages
// collapsed into one entry (from `poly-lsp-mcp status --json`).
type Root struct {
	Root     string   `json:"root"`
	Langs    []string `json:"langs"`
	Children int      `json:"children"` // language servers it has an entry for
	Running  int      `json:"running"`  // of those, running now
	IdleSec  int64    `json:"idle_sec"` // least idle of its languages; -1 none
	Files    int64    `json:"files"`    // indexed; -1 unknown
	Bytes    int64    `json:"bytes"`
	// Group and Session: the dun session owning the root, else GroupNone.
	Group   string `json:"group"`
	Session string `json:"session"`
}

// statusEntry is the part of one `status --json` element read here.
type statusEntry struct {
	PID      int    `json:"pid"`
	Root     string `json:"root"`
	Children []struct {
		Language string `json:"language"`
		State    string `json:"state"`
		IdleSec  *int64 `json:"idleSec"`
	} `json:"children"`
	Index *struct {
		Files int64 `json:"files"`
		Bytes int64 `json:"bytes"`
	} `json:"index"`
}

// ParseStatus reads `poly-lsp-mcp status --json`, keeping the entries of
// daemon pid (0: all), one Root each in the daemon's order.
func ParseStatus(data []byte, pid int) ([]Root, error) {
	var es []statusEntry
	if err := json.Unmarshal(data, &es); err != nil {
		return nil, fmt.Errorf("not the status JSON: %w", err)
	}
	var out []Root
	for _, e := range es {
		if pid != 0 && e.PID != pid {
			continue
		}
		r := Root{Root: e.Root, IdleSec: -1, Files: -1, Group: GroupNone, Session: GroupNone}
		for _, c := range e.Children {
			r.Children++
			if c.State == "running" {
				r.Running++
			}
			if c.Language != "" && !contains(r.Langs, c.Language) {
				r.Langs = append(r.Langs, c.Language)
			}
			if c.IdleSec != nil && (r.IdleSec < 0 || *c.IdleSec < r.IdleSec) {
				r.IdleSec = *c.IdleSec
			}
		}
		if e.Index != nil {
			r.Files, r.Bytes = e.Index.Files, e.Index.Bytes
		}
		out = append(out, r)
	}
	return out, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// PolyDaemon is the shared poly-lsp-mcp daemon in snap, nil if none.
func PolyDaemon(snap Snapshot) *Proc {
	for _, pid := range sortedPIDs(snap) {
		if p := snap.Procs[pid]; Role(p.Argv) == rolePolyDaemon {
			return p
		}
	}
	return nil
}

// ReadRoots runs the daemon's own binary (else poly-lsp-mcp on PATH) as
// `status --json` for the daemon's home, with a StatusTimeout.
func ReadRoots(d *Proc) ([]Root, error) {
	bin := "poly-lsp-mcp"
	if len(d.Argv) > 0 && filepath.IsAbs(d.Argv[0]) {
		bin = d.Argv[0]
	}
	args := []string{"status", "--json"}
	for i, a := range d.Argv {
		if v, ok := strings.CutPrefix(a, "--daemon-home="); ok {
			args = append(args, "--daemon-home="+v)
		} else if a == "--daemon-home" && i+1 < len(d.Argv) {
			args = append(args, "--daemon-home="+d.Argv[i+1])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), StatusTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = 200 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("no answer in %s", StatusTimeout)
	}
	if err != nil {
		if m := strings.TrimSpace(stderr.String()); m != "" {
			return nil, fmt.Errorf("%s", textfmt.OneLine(m))
		}
		return nil, err
	}
	return ParseStatus(out, d.PID)
}

// AttachRoots gives the daemon's row its roots, each attributed to the
// session whose workspace, worktree or sub-agent tree holds it (as the
// language servers are), or statusErr when they could not be read.
func AttachRoots(rows []Row, daemonPID int, roots []Root, statusErr error, list *proto.ProcsList) {
	for i := range rows {
		if rows[i].PID != daemonPID {
			continue
		}
		if statusErr != nil {
			rows[i].RootsErr = "poly-lsp-mcp status failed: " + statusErr.Error()
			return
		}
		var sessions []proto.ProcSession
		if list != nil {
			sessions = list.Sessions
		}
		rs := make([]Root, len(roots))
		copy(rs, roots)
		for j := range rs {
			if s, ok := Owner(rs[j].Root, sessions); ok {
				rs[j].Group, rs[j].Session = s.ID, SessionLabel(s.ID, s.Workspace)
			}
		}
		// Owned roots first; the daemon's order within each.
		sort.SliceStable(rs, func(a, b int) bool {
			return rs[a].Group != GroupNone && rs[b].Group == GroupNone
		})
		rows[i].Roots = rs
		return
	}
}

// RootNames shortens each root to its base name; two roots with one base
// name keep their ~ path, which the table elides in the middle.
func RootNames(rs []Root) []string {
	count := map[string]int{}
	for _, r := range rs {
		count[filepath.Base(r.Root)]++
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		if b := filepath.Base(r.Root); count[b] == 1 {
			out[i] = b
		} else {
			out[i] = Tilde(r.Root)
		}
	}
	return out
}

// RootLine is what the JOB column says of a root.
func RootLine(r Root, name string) string {
	langs := "-"
	if len(r.Langs) > 0 {
		langs = strings.Join(shortLangs(r.Langs), ",")
	}
	s := "root " + name + "  langs " + langs
	if r.Children == 0 {
		s += "  no child LSPs"
	} else {
		s += fmt.Sprintf("  children %d running", r.Running)
	}
	if r.Files >= 0 {
		s += fmt.Sprintf("  index %d files %s", r.Files, mb(r.Bytes))
	}
	if r.IdleSec >= 0 {
		s += "  idle " + Age(r.IdleSec)
	}
	if r.Group == GroupNone {
		s += "  kept, no client"
	}
	return s
}

func shortLangs(ls []string) []string {
	short := map[string]string{"typescript": "ts", "javascript": "js", "python": "py", "rust": "rs"}
	out := make([]string, len(ls))
	for i, l := range ls {
		if s, ok := short[l]; ok {
			l = s
		}
		out[i] = l
	}
	return out
}

func mb(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}
