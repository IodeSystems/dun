package dun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A process the agent backgrounded outlives Close (session end): a server a
// task says to leave running must still be up after dun exits. The shell
// itself is gone.
func TestHostShell_CloseLeavesBackgroundedProcess(t *testing.T) {
	dir := t.TempDir()
	hs := &HostShell{Dir: dir}
	r := hs.Run(context.Background(), "nohup sleep 30 > /dev/null 2>&1 & echo $! > bg.pid; echo $$", nil)
	if r.Failed() {
		t.Fatalf("run: %v", r)
	}
	shPid, _ := strconv.Atoi(strings.TrimSpace(r.Output))
	b, err := os.ReadFile(filepath.Join(dir, "bg.pid"))
	if err != nil {
		t.Fatal(err)
	}
	bg, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { _ = syscall.Kill(bg, syscall.SIGKILL) })

	hs.Close()

	// A killed process can linger as a zombie until its new parent reaps it,
	// and kill(pid, 0) succeeds on a zombie: read its state instead.
	time.Sleep(300 * time.Millisecond)
	if !running(bg) {
		t.Fatalf("backgrounded pid %d was killed by Close", bg)
	}
	deadline := time.Now().Add(2 * time.Second)
	for running(shPid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if running(shPid) {
		t.Fatalf("shell pid %d still alive after Close", shPid)
	}
}

// running: the pid exists and is not a zombie.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}
