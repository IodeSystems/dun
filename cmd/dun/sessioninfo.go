package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun"
)

// `dun session <id> info [--context]` (USER 2026-10-27): what dun is in a
// session, as JSON, for a person or a script that has to say (a repo's PR
// rules). It reads the facts the session's engine keeps beside its log
// (<id>.harness.json, dun/harnessinfo.go), so it answers for a running
// session and a finished one alike. With no id, DUN_SESSION_ID: a command
// dun runs has it in its environment.
func sessionCmd(args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: dun session [<id>] info [--context]   (no id: $DUN_SESSION_ID)")
		return 2
	}
	var id string
	withContext := false
	sawInfo := false
	for _, a := range args {
		switch {
		case a == "info":
			sawInfo = true
		case a == "--context":
			withContext = true
		case strings.HasPrefix(a, "-"):
			return usage()
		case id == "":
			id = a
		default:
			return usage()
		}
	}
	if !sawInfo {
		return usage()
	}
	if id == "" {
		id = os.Getenv("DUN_SESSION_ID")
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "dun session info: which session? give its id, or run it from a command dun runs ($DUN_SESSION_ID)")
		return 2
	}
	matches, _ := filepath.Glob(filepath.Join(dunHome(), "sessions", "*", id+".harness.json"))
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "dun session info: no session %s with harness facts in %s (a session started before this dun keeps none)\n",
			id, filepath.Join(dunHome(), "sessions"))
		return 1
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "dun session info:", err)
		return 1
	}
	var in dun.HarnessInfo
	if err := json.Unmarshal(b, &in); err != nil {
		fmt.Fprintln(os.Stderr, "dun session info:", err)
		return 1
	}
	if !withContext {
		in.Context = nil
	}
	out, _ := json.MarshalIndent(in, "", "  ")
	fmt.Println(string(out))
	return 0
}
