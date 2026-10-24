package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/dundcli"
)

// The first exit says, once per dun home, that dund stays running and how
// to stop it; the next exit says nothing. With no dund running, nothing.
func TestNoteBackground_OncePerHome(t *testing.T) {
	home, err := os.MkdirTemp("", "dun-nb-") // short: a unix socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("DUN_HOME", home)
	var b bytes.Buffer
	noteBackground(&b)
	if b.Len() != 0 {
		t.Fatalf("no dund, but: %s", b.String())
	}
	sock := dundcli.Socket(home)
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	noteBackground(&b)
	s := b.String()
	// The stop step is the pidfile's, or the service's on a machine with
	// the service installed (ServiceFile reads the real unit directory).
	stop := strings.Contains(s, "dund.pid") || strings.Contains(s, "dun dund service stop")
	if !strings.Contains(s, "keeps running in the background") || !stop || !strings.Contains(s, "dun -d shutdown") {
		t.Errorf("first exit:\n%s", s)
	}
	b.Reset()
	noteBackground(&b)
	if b.Len() != 0 {
		t.Errorf("said twice: %s", b.String())
	}
}
