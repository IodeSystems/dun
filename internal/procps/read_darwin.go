package procps

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IONote says what the IO column can show here.
const IONote = ""

// Read asks ps for the table (pid, parent, group, memory, CPU time, age and
// command line), takes each argv from kern.procargs2 (ps joins it with
// spaces), and the disk IO from proc_pid_rusage.
func Read() (Snapshot, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,pgid=,rss=,time=,etime=,command=").Output()
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now()
	starts := startTimes()
	snap := Snapshot{At: now, Procs: map[int]*Proc{}}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid := atoi(f[0])
		if pid <= 0 {
			continue
		}
		p := &Proc{PID: pid, PPID: atoi(f[1]), PGID: atoi(f[2]), RSS: int64(atoi(f[3])) * 1024,
			CPU: clock(f[4]), Read: -1, Write: -1}
		if st, ok := starts[pid]; ok {
			p.Start = st
		} else if e := clock(f[5]); e >= 0 {
			p.Start = now.Add(-time.Duration(e) * time.Second)
		}
		if argv := procArgs(pid); len(argv) > 0 {
			p.Argv = argv
		} else {
			p.Argv = f[6:]
		}
		if r, w, ok := diskIO(pid); ok {
			p.Read, p.Write = r, w
		}
		snap.Procs[pid] = p
	}
	return snap, nil
}

// startTimes is each process's start from sysctl kern.proc.all, exact to
// the microsecond, so Rates can tell a reused pid.
func startTimes() map[int]time.Time {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	out := make(map[int]time.Time, len(ks))
	for _, k := range ks {
		t := k.Proc.P_starttime
		out[int(k.Proc.P_pid)] = time.Unix(int64(t.Sec), int64(t.Usec)*1000)
	}
	return out
}

// procArgs is the argv from sysctl kern.procargs2: argc, the exec path, NUL
// padding, then argc NUL-ended arguments.
func procArgs(pid int) []string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	var argv []string
	for len(argv) < argc && len(b) > 0 {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			argv = append(argv, string(b))
			break
		}
		argv = append(argv, string(b[:j]))
		b = b[j+1:]
	}
	return argv
}

// proc_info (libproc's syscall, sys/proc_info.h).
const (
	procInfoCallPidinfo    = 2
	procInfoCallPidrusage  = 9
	procPidVnodePathInfo   = 9
	rusageInfoV2           = 2
	vnodeInfoSize          = 152  // struct vnode_info
	maxPathLen             = 1024 // MAXPATHLEN
	vnodePathInfoSize      = 2 * (vnodeInfoSize + maxPathLen)
	rusageV2DiskReadOffset = 16 + 16*8 // uuid, then 16 uint64 fields
)

func procInfo(call, pid, flavor int, buf unsafe.Pointer, size int) (int, bool) {
	r, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), 0,
		uintptr(buf), uintptr(size))
	return int(r), errno == 0
}

// diskIO is proc_pid_rusage's ri_diskio_bytesread and _byteswritten.
func diskIO(pid int) (read, write int64, ok bool) {
	// On the heap, so it cannot move during the call. rusage_info_v2 is
	// 160 bytes; the rest is room.
	buf := make([]uint64, 64)
	if _, ok := procInfo(procInfoCallPidrusage, pid, rusageInfoV2, unsafe.Pointer(&buf[0]), 0); !ok {
		return 0, 0, false
	}
	return int64(buf[rusageV2DiskReadOffset/8]), int64(buf[rusageV2DiskReadOffset/8+1]), true
}

// Cwd is the process's working directory: proc_pidinfo
// PROC_PIDVNODEPATHINFO, else lsof ("" unknown).
func Cwd(pid int) string {
	buf := make([]byte, vnodePathInfoSize)
	if n, ok := procInfo(procInfoCallPidinfo, pid, procPidVnodePathInfo, unsafe.Pointer(&buf[0]), len(buf)); ok && n == len(buf) {
		path := buf[vnodeInfoSize : vnodeInfoSize+maxPathLen]
		if i := bytes.IndexByte(path, 0); i > 0 {
			return string(path[:i])
		}
	}
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "n"); ok {
			return v
		}
	}
	return ""
}

// clock parses ps's times, [[dd-]hh:]mm:ss[.hh] and mm:ss.hh, to seconds;
// -1 unparseable.
func clock(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, s = float64(atoi(d)), rest
	}
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return -1
		}
		total = total*60 + v
	}
	return days*86400 + total
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
