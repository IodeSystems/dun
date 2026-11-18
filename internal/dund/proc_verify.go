package dund

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Whether a pid is still the engine an entry names, before dund kills it
// (an engine that is not adopted) or lets its session start another: a pid
// can be reused by any program once the engine is gone.

type engineCheck int

const (
	engineUnknown engineCheck = iota // cannot tell
	engineIs                         // its argv is an engine's on this dir, and its start fits
	engineNot                        // another program
)

// verifyEngine: the process's argv has --engine and --listen dir, and, where
// its start time can be read (Linux), it started near the entry's start.
func verifyEngine(pid int, dir, started string) engineCheck {
	argv, err := procArgv(pid)
	if err != nil || len(argv) == 0 {
		return engineUnknown
	}
	joined := " " + strings.Join(argv, " ") + " "
	if !slices.Contains(argv, "--engine") ||
		!(strings.Contains(joined, " --listen "+dir+" ") || strings.Contains(joined, " --listen="+dir+" ")) {
		return engineNot
	}
	if at, err := time.Parse(time.RFC3339, started); err == nil {
		if st, ok := procStart(pid); ok && (st.After(at.Add(5*time.Second)) || st.Before(at.Add(-5*time.Minute))) {
			return engineNot
		}
	}
	return engineIs
}

// procArgv is a process's command line: /proc on Linux, ps elsewhere (its
// words split on spaces: enough to find the flags).
func procArgv(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// procStart is when a process started (Linux: /proc/<pid>/stat's start
// time in clock ticks after boot, at the usual 100 a second).
func procStart(pid int) (time.Time, bool) {
	if runtime.GOOS != "linux" {
		return time.Time{}, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return time.Time{}, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 { // starttime is field 22: the 20th after the name
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	st, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(st), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100), true
		}
	}
	return time.Time{}, false
}
