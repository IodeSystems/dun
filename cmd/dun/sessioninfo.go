package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
)

// `dun session <id> info [--context]` (USER 2026-10-27): what dun is in a
// session, as JSON, for a person or a script that has to say (a repo's PR
// rules). It reads the facts the session's engine keeps beside its log
// (<id>.harness.json, dun/harnessinfo.go), so it answers for a running
// session and a finished one alike. With no id, DUN_SESSION_ID: a command
// dun runs has it in its environment, and a sub-agent's commands have the
// sub-agent's own (withExecEnv), so each reads its own model and effort.
//
// `dun session [<id>] attribution [--format git-trailer|kv|json]` is the
// same record cut to what a commit or PR names: harness, version, model,
// effort. An agent types those from memory and gets the model wrong; this
// reads them. A field the record lacks is an error, never a guess.
const sessionUsage = `usage: dun session [<id>] info [--context]
       dun session [<id>] attribution [--format git-trailer|kv|json]
  no id: $DUN_SESSION_ID (a command dun runs has it)

info         the session's harness facts, as JSON
attribution  the agent line for a commit or PR, read from the session's record:
  git-trailer  Agent: dun v0.4.3 (model <model_id>, effort <effort>)   (default)
  kv           Agent-Harness / Agent-Harness-Version / Agent-Model / Agent-Effort lines
  json         {"harness","harness_version","model_id","effort"}
  A dev build prints "dev" as its version. An unknown field is an error.`

func sessionCmd(args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, sessionUsage)
		return 2
	}
	var id, verb, format string
	withContext := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(sessionUsage)
			return 0
		case (a == "info" || a == "attribution") && verb == "":
			verb = a
		case a == "--context":
			withContext = true
		case a == "--format" && i+1 < len(args):
			i++
			format = args[i]
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case strings.HasPrefix(a, "-"):
			return usage()
		case id == "":
			id = a
		default:
			return usage()
		}
	}
	if verb == "" || (verb == "info" && format != "") || (verb == "attribution" && withContext) {
		return usage()
	}
	if format == "" {
		format = "git-trailer"
	}
	if verb == "attribution" && format != "git-trailer" && format != "kv" && format != "json" {
		fmt.Fprintf(os.Stderr, "dun session attribution: format %q: want git-trailer, kv or json\n", format)
		return 2
	}
	name := "dun session " + verb
	if id == "" {
		id = os.Getenv("DUN_SESSION_ID")
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "%s: which session? give its id, or run it from a command dun runs ($DUN_SESSION_ID)\n", name)
		return 2
	}
	matches, _ := filepath.Glob(filepath.Join(dunconfig.DunHome(), "sessions", "*", id+".harness.json"))
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no session %s with harness facts in %s (a session started before this dun keeps none)\n",
			name, id, filepath.Join(dunconfig.DunHome(), "sessions"))
		return 1
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 1
	}
	var in dun.HarnessInfo
	if err := json.Unmarshal(b, &in); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 1
	}
	if verb == "attribution" {
		out, err := attribution(in, format)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			return 1
		}
		fmt.Println(out)
		return 0
	}
	if !withContext {
		in.Context = nil
	}
	out, _ := json.MarshalIndent(in, "", "  ")
	fmt.Println(string(out))
	return 0
}

// attribution renders the record as what a commit or PR names. Every field
// must be known: a guess in a commit is worse than a refusal.
func attribution(in dun.HarnessInfo, format string) (string, error) {
	f := []struct{ name, val string }{
		{"harness", in.Harness}, {"harness_version", in.Version}, {"model_id", in.Model}, {"effort", in.Effort},
	}
	for _, x := range f {
		if strings.TrimSpace(x.val) == "" {
			return "", fmt.Errorf("session %s has no %s in its record; not guessing", in.SessionID, x.name)
		}
	}
	switch format {
	case "kv":
		return fmt.Sprintf("Agent-Harness: %s\nAgent-Harness-Version: %s\nAgent-Model: %s\nAgent-Effort: %s",
			in.Harness, in.Version, in.Model, in.Effort), nil
	case "json":
		out, _ := json.Marshal(struct {
			Harness        string `json:"harness"`
			HarnessVersion string `json:"harness_version"`
			ModelID        string `json:"model_id"`
			Effort         string `json:"effort"`
		}{in.Harness, in.Version, in.Model, in.Effort})
		return string(out), nil
	}
	return fmt.Sprintf("Agent: %s %s (model %s, effort %s)", in.Harness, in.Version, in.Model, in.Effort), nil
}
