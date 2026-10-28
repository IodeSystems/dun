// Package procstat reads what a process group is doing: each process's
// state, CPU, memory, threads, open files, IO and where in the kernel it is
// waiting. It is how a background job is told apart as busy, waiting on IO,
// or stuck (USER 2026-10-28: "so we can detect cpu/io/deadlock situations").
//
// A Sampler keeps the previous reading of each process, so a rate (CPU%,
// bytes a second) is the change since the last Sample of the same group.
// The first Sample of a group has no rates yet: Rated is false.
//
// Linux reads /proc and has every field. Elsewhere ps gives state, CPU%,
// memory and wchan; open files and IO are unknown (FDs and the IO rates -1).
package procstat

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proc is one process of the group.
type Proc struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	Name    string  `json:"name"`
	State   string  `json:"state"`   // R running, S sleeping, D waiting on IO, Z zombie, T stopped, …
	CPU     float64 `json:"cpu"`     // percent of one core since the last sample; -1 before a rate
	RSS     int64   `json:"rss"`     // resident bytes
	Threads int     `json:"threads"` // 0 unknown
	FDs     int     `json:"fds"`     // open file descriptors; -1 unknown
	ReadBps float64 `json:"read_bps"`
	WritBps float64 `json:"write_bps"` // bytes a second to storage; -1 unknown or before a rate
	WChan   string  `json:"wchan,omitempty"`
	// Context switches since the last sample: voluntary (it waited) and not
	// (it was preempted). -1 unknown or before a rate.
	VolCS   int64 `json:"vol_cs"`
	InvolCS int64 `json:"invol_cs"`
}

// Usage is a sample of the whole group.
type Usage struct {
	At    time.Time `json:"at"`
	PGID  int       `json:"pgid"`
	Procs []Proc    `json:"procs"`
	Rated bool      `json:"rated"` // rates are since an earlier sample
	// Totals over the group; FDs and the IO rates are -1 when unknown.
	CPU     float64 `json:"cpu"`
	RSS     int64   `json:"rss"`
	Threads int     `json:"threads"`
	FDs     int     `json:"fds"`
	ReadBps float64 `json:"read_bps"`
	WritBps float64 `json:"write_bps"`
}

// raw is one process's counters, for the next rate.
type raw struct {
	cpu         float64 // seconds of CPU
	read, write uint64
	vol, invol  uint64
	ioKnown     bool
	at          time.Time
}

// Sampler samples groups and keeps what a rate needs: each group's last
// reading, by pid. One Sampler may serve many groups.
type Sampler struct {
	mu   sync.Mutex
	prev map[int]map[int]raw // pgid → pid → counters
}

// Sample reads process group pgid. An empty Procs means nothing is left in
// it (or the platform cannot say).
func (s *Sampler) Sample(pgid int) Usage {
	now := time.Now()
	procs, raws := readGroup(pgid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev == nil {
		s.prev = map[int]map[int]raw{}
	}
	last := s.prev[pgid]
	u := Usage{At: now, PGID: pgid, Rated: len(procs) > 0}
	next := map[int]raw{}
	for i := range procs {
		p := &procs[i]
		r := raws[i]
		r.at = now
		next[p.PID] = r
		old, had := last[p.PID]
		if had && now.Sub(old.at) > 0 && p.CPU < 0 {
			dt := now.Sub(old.at).Seconds()
			p.CPU = 100 * (r.cpu - old.cpu) / dt
			if r.ioKnown {
				p.ReadBps = float64(r.read-old.read) / dt
				p.WritBps = float64(r.write-old.write) / dt
			}
			p.VolCS, p.InvolCS = int64(r.vol-old.vol), int64(r.invol-old.invol)
		} else if !had && p.CPU < 0 {
			u.Rated = false
		}
	}
	if len(procs) == 0 {
		delete(s.prev, pgid) // the group is gone
	} else {
		s.prev[pgid] = next // exited processes drop out
	}
	u.Procs = procs
	fdsKnown, ioKnown := true, true
	for _, p := range procs {
		if p.CPU > 0 {
			u.CPU += p.CPU
		}
		u.RSS += p.RSS
		u.Threads += p.Threads
		if p.FDs < 0 {
			fdsKnown = false
		} else {
			u.FDs += p.FDs
		}
		if p.ReadBps < 0 || p.WritBps < 0 {
			ioKnown = false
		} else {
			u.ReadBps += p.ReadBps
			u.WritBps += p.WritBps
		}
	}
	if !fdsKnown {
		u.FDs = -1
	}
	if !ioKnown {
		u.ReadBps, u.WritBps = -1, -1
	}
	sort.Slice(u.Procs, func(i, j int) bool { return u.Procs[i].PID < u.Procs[j].PID })
	return u
}

// Forget drops what the Sampler kept for group pgid.
func (s *Sampler) Forget(pgid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prev, pgid)
}

// States counts the processes by state: "R2 S1".
func (u Usage) States() string {
	n := map[string]int{}
	for _, p := range u.Procs {
		n[p.State]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s%d", k, n[k])
	}
	return b.String()
}

// Line is the group in one line: cpu 97% · 3 procs (R2 S1) · 412 MB ·
// 23 fds · io r 0 B/s w 1.2 MB/s.
func (u Usage) Line() string {
	if len(u.Procs) == 0 {
		return "no processes left in its group"
	}
	var parts []string
	if u.Rated {
		parts = append(parts, fmt.Sprintf("cpu %.0f%%", u.CPU))
	}
	parts = append(parts, fmt.Sprintf("%d proc%s (%s)", len(u.Procs), plural(len(u.Procs)), u.States()),
		Bytes(float64(u.RSS)))
	if u.Threads > 0 {
		parts = append(parts, fmt.Sprintf("%d thread%s", u.Threads, plural(u.Threads)))
	}
	if u.FDs >= 0 {
		parts = append(parts, fmt.Sprintf("%d fds", u.FDs))
	}
	if u.Rated && u.ReadBps >= 0 {
		parts = append(parts, "io r "+Bytes(u.ReadBps)+"/s w "+Bytes(u.WritBps)+"/s")
	}
	return strings.Join(parts, " · ")
}

// Verdict reads the sample: what the group looks like it is doing. quiet is
// how long it has printed nothing (0 unknown).
func (u Usage) Verdict(quiet time.Duration) string {
	if len(u.Procs) == 0 {
		return "nothing is running in its process group"
	}
	var d, z, live int
	var dWchan []string
	for _, p := range u.Procs {
		switch p.State {
		case "D":
			d++
			if p.WChan != "" && p.WChan != "0" {
				dWchan = append(dWchan, p.WChan)
			}
		case "Z":
			z++
		}
		if p.State != "Z" {
			live++
		}
	}
	switch {
	case live == 0:
		return "only zombies left: its processes exited and were not reaped"
	case d > 0:
		v := fmt.Sprintf("%d waiting on disk or IO (D)", d)
		if len(dWchan) > 0 {
			v += " in " + strings.Join(uniq(dWchan), ", ")
		}
		return v
	case !u.Rated:
		return ""
	case u.CPU >= 50:
		if quiet >= time.Minute {
			return fmt.Sprintf("busy on CPU, printing nothing for %s", short(quiet))
		}
		return "busy on CPU"
	case u.CPU < 1 && (u.ReadBps < 0 || u.ReadBps+u.WritBps < 1024):
		v := "idle: every process sleeping, no CPU"
		if u.ReadBps >= 0 {
			v += ", no IO"
		}
		if w := waits(u); w != "" {
			v += " (waiting in " + w + ")"
		}
		if quiet >= 2*time.Minute {
			v += fmt.Sprintf(", no output for %s — looks stuck", short(quiet))
		}
		return v
	}
	return ""
}

// waits names where the sleeping processes wait, most common first.
func waits(u Usage) string {
	var w []string
	for _, p := range u.Procs {
		if p.WChan != "" && p.WChan != "0" && p.WChan != "-" {
			w = append(w, p.WChan)
		}
	}
	w = uniq(w)
	if len(w) > 3 {
		w = w[:3]
	}
	return strings.Join(w, ", ")
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func short(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Bytes is a size the short way: 0 B, 812 B, 4.1 KB, 412 MB, 1.2 GB.
func Bytes(n float64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%.0f B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", n/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.0f MB", n/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", n/(1024*1024*1024))
}
