//go:build unix

package dund

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An engine whose child holds its stdout open (an MCP server, a job) used to
// keep a stop waiting for EOF forever: the dund kept its pidfile and the next
// one was refused as "already running". Killing the group closes it.
func TestKillReachesTheEnginesChildren(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "eng")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 600 &\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng, err := ExecSpawner{Bin: bin}.Spawn(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	eof := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, eng.Stdout()); close(eof) }()
	time.Sleep(100 * time.Millisecond) // the child is started
	eng.Kill()
	select {
	case <-eof:
	case <-time.After(5 * time.Second):
		t.Fatal("stdout still open after Kill: a child outlived the engine")
	}
	_ = eng.Wait()
}
