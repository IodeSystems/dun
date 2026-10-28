//go:build !linux

package procstat

import (
	"os/exec"
	"strconv"
	"strings"
)

// readGroup asks ps for every process and keeps pgid's. ps gives state,
// CPU% (its own decaying average, so CPU is set and not a rate), memory and
// wchan; open files, threads and IO are unknown.
func readGroup(pgid int) ([]Proc, []raw) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,stat=,%cpu=,rss=,wchan=,comm=").Output()
	if err != nil {
		return nil, nil
	}
	var procs []Proc
	var raws []raw
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		if n, _ := strconv.Atoi(f[2]); n != pgid {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		cpu, _ := strconv.ParseFloat(f[4], 64)
		rss, _ := strconv.ParseInt(f[5], 10, 64)
		procs = append(procs, Proc{PID: pid, PPID: ppid, Name: strings.Join(f[7:], " "), State: f[3][:1],
			CPU: cpu, RSS: rss * 1024, FDs: -1, ReadBps: -1, WritBps: -1, VolCS: -1, InvolCS: -1, WChan: f[6]})
		raws = append(raws, raw{})
	}
	return procs, raws
}
