package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iodesystems/dun/internal/dundcli"
)

// Notices said once per install (per dun home), not once per process as
// once() in upgrade.go is: a marker file under $DUN_HOME/notices records that
// it was said.

// onceEver reports whether the notice key has never been said in this dun
// home, and records it. A home it cannot write to says nothing: a notice
// repeated on every run is worse than one not said.
func onceEver(key string) bool {
	dir := filepath.Join(dunHome(), "notices")
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, key), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// noteBackground is said after the first /exit: dund and the launcher stay
// up after the TUI is gone, and nothing on screen said so.
func noteBackground(w io.Writer) {
	if !dialOK(dundcli.Socket(dunHome())) || !onceEver("dund-background") {
		return
	}
	stop := killLine()
	if _, svc := dundcli.ServiceFile(dundcli.ServiceUnit); svc {
		stop = "dun dund service stop"
	}
	fmt.Fprintf(w, "dun: dund, the session service, keeps running in the background; stop it: %s\n"+
		"     (the launcher, dun -d, exits after %d idle minutes; dun -d shutdown stops it now)\n", stop, int(idleTTL.Minutes()))
}
