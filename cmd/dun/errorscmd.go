package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
)

// `dun errors`: the turn errors dun kept (internal/harness/turnerrors.go),
// newest last, from this machine's $DUN_HOME. It needs no dund and no
// provider, so it runs where the error happened and its output can be pasted
// (aw4 i_1m7rqbptp6z6f22q6jgff8).

const errorsHelp = `dun errors — the turn errors dun kept, newest last

  dun errors [--session <id>] [--json] [-n N]

Each turn that ends in an error leaves a record beside its session
(<session>.errors.jsonl, the newest 200 per session): the time (UTC), the
session, the provider, the endpoint's host, the model, the HTTP status when
there was one, the error and the server's body as agentkit kept it, at most
8 KiB. Keys and Authorization headers are replaced with [redacted].

Flags:

  --session <id>  only this session's errors (and its sub-agents')
  --json          the records as a JSON array
  -n N            the newest N (default 20; 0 = all)
  -h, --help      this text
`

func errorsCmd(args []string, out, errw io.Writer) int {
	var session string
	asJSON, n := false, 20
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(name string) (string, bool) {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v, true
			}
			if i+1 >= len(args) {
				fmt.Fprintf(errw, "dun errors: %s needs a value\n\n%s", name, errorsHelp)
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprint(out, errorsHelp)
			return 0
		case a == "--json":
			asJSON = true
		case a == "--session" || strings.HasPrefix(a, "--session="):
			v, ok := value("--session")
			if !ok {
				return 2
			}
			session = v
		case a == "-n" || strings.HasPrefix(a, "-n="):
			v, ok := value("-n")
			if !ok {
				return 2
			}
			k, err := strconv.Atoi(v)
			if err != nil || k < 0 {
				fmt.Fprintf(errw, "dun errors: -n %q is not a count\n", v)
				return 2
			}
			n = k
		default:
			fmt.Fprintf(errw, "dun errors: unknown argument %q\n\n%s", a, errorsHelp)
			return 2
		}
	}
	recs, err := harness.AllTurnErrors(session)
	if err != nil {
		fmt.Fprintln(errw, "dun errors:", err)
		return 1
	}
	if n > 0 && len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	if asJSON {
		if recs == nil {
			recs = []harness.TurnError{}
		}
		b, _ := json.MarshalIndent(recs, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	if len(recs) == 0 {
		where := harness.SessionsDir()
		if session != "" {
			where = "session " + session + " in " + where
		}
		fmt.Fprintf(out, "no turn errors kept in %s\n", where)
		return 0
	}
	for i, r := range recs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, errorsHead(r))
		fmt.Fprintln(out, "  "+indentRest(r.Error))
		if r.Payload != "" {
			fmt.Fprintln(out, "  payload: "+indentRest(r.Payload))
		}
		if r.Metadata != "" {
			fmt.Fprintln(out, "  metadata: "+indentRest(r.Metadata))
		}
	}
	return 0
}

// errorsHead is a record's first line: when, where, which model.
func errorsHead(r harness.TurnError) string {
	parts := []string{r.Time.UTC().Format(time.RFC3339), r.Session}
	for _, p := range []string{r.Provider, r.Host, r.Model} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if r.Status != 0 {
		parts = append(parts, "status "+strconv.Itoa(r.Status))
	}
	for _, kv := range [][2]string{{"code", r.Code}, {"type", r.Type}, {"finish", r.Finish},
		{"native", r.NativeFinish}, {"stall", r.Stall}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " · ")
}

// indentRest keeps a multi-line text under its first line.
func indentRest(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
