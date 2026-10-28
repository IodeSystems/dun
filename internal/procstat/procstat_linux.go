package procstat

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// clkTck is the kernel's clock ticks a second for /proc's CPU times. 100 on
// every Linux build dun runs on (sysconf(_SC_CLK_TCK)); reading it needs cgo.
const clkTck = 100

// readGroup reads every process whose process group is pgid from /proc.
// A process that exits mid-read is skipped.
func readGroup(pgid int) ([]Proc, []raw) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	page := int64(os.Getpagesize())
	var procs []Proc
	var raws []raw
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
		// comm is in parentheses and may hold spaces and parentheses itself:
		// the fields start after the LAST ")".
		s := string(st)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < open {
			continue
		}
		f := strings.Fields(s[close+1:])
		// f[0] is field 3 (state); field n is f[n-3].
		if len(f) < 22 || atoi(f[2]) != pgid {
			continue
		}
		p := Proc{PID: pid, PPID: atoi(f[1]), Name: s[open+1 : close], State: f[0], CPU: -1,
			Threads: atoi(f[17]), RSS: int64(atoi(f[21])) * page, FDs: -1, ReadBps: -1, WritBps: -1,
			VolCS: -1, InvolCS: -1}
		r := raw{cpu: float64(atou(f[11])+atou(f[12])) / clkTck}
		if fds, err := os.ReadDir(dir + "/fd"); err == nil {
			p.FDs = len(fds)
		}
		if b, err := os.ReadFile(dir + "/wchan"); err == nil {
			p.WChan = strings.TrimSpace(string(b))
		}
		if kv := keyVals(dir + "/io"); kv != nil {
			r.read, r.write, r.ioKnown = kv["read_bytes"], kv["write_bytes"], true
		}
		if kv := keyVals(dir + "/status"); kv != nil {
			r.vol, r.invol = kv["voluntary_ctxt_switches"], kv["nonvoluntary_ctxt_switches"]
		}
		procs = append(procs, p)
		raws = append(raws, r)
	}
	return procs, raws
}

// keyVals reads a "key: number" file (/proc/<pid>/io, status); nil if it
// cannot be read (another user's process: io needs the same user).
func keyVals(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.Fields(v + " x")[0], 10, 64); err == nil {
			out[strings.TrimSpace(k)] = n
		}
	}
	return out
}

func atoi(s string) int    { n, _ := strconv.Atoi(s); return n }
func atou(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
