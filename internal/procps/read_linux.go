package procps

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// clkTck is the kernel's clock ticks a second for /proc's times (see
// internal/procstat: sysctl needs cgo, and every Linux dun runs on has 100).
const clkTck = 100

// IONote says what the IO column can show here.
const IONote = ""

// Read reads the process table from /proc. A process that exits mid-read
// is skipped; kernel threads (no argv) are left out.
func Read() (Snapshot, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return Snapshot{}, err
	}
	boot := bootTime()
	page := int64(os.Getpagesize())
	snap := Snapshot{At: time.Now(), Procs: map[int]*Proc{}}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		st, err := os.ReadFile(dir + "/stat")
		if err != nil {
			continue
		}
		s := string(st)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < open {
			continue
		}
		f := strings.Fields(s[close+1:])
		// f[0] is field 3 (state); field n is f[n-3].
		if len(f) < 22 {
			continue
		}
		cl, _ := os.ReadFile(dir + "/cmdline")
		cl = bytes.TrimRight(cl, "\x00")
		if len(cl) == 0 {
			continue // a kernel thread, or a zombie
		}
		p := &Proc{PID: pid, PPID: atoi(f[1]), PGID: atoi(f[2]),
			Argv: strings.Split(string(cl), "\x00"),
			CPU:  float64(atoi(f[11])+atoi(f[12])) / clkTck,
			RSS:  int64(atoi(f[21])) * page, Read: -1, Write: -1}
		if !boot.IsZero() {
			p.Start = boot.Add(time.Duration(atoi(f[19])) * time.Second / clkTck)
		}
		if r, w, ok := ioBytes(dir + "/io"); ok {
			p.Read, p.Write = r, w
		}
		snap.Procs[pid] = p
	}
	return snap, nil
}

// Cwd is the process's working directory ("" unknown).
func Cwd(pid int) string {
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return p
}

func bootTime() time.Time {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			return time.Unix(int64(atoi(strings.TrimSpace(v))), 0)
		}
	}
	return time.Time{}
}

// ioBytes is /proc/<pid>/io's storage counters.
func ioBytes(path string) (read, write int64, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	var nr, nw bool
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch k {
		case "read_bytes":
			read, nr = int64(atoi(strings.TrimSpace(v))), true
		case "write_bytes":
			write, nw = int64(atoi(strings.TrimSpace(v))), true
		}
	}
	return read, write, nr && nw
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
