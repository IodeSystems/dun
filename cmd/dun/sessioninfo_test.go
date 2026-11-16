package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runSession runs `dun session args…` and returns its code, stdout and stderr.
func runSession(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	capture := func(f **os.File) (restore func() string) {
		old := *f
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		*f = w
		return func() string {
			w.Close()
			*f = old
			var b bytes.Buffer
			io.Copy(&b, r)
			return b.String()
		}
	}
	outDone, errDone := capture(&os.Stdout), capture(&os.Stderr)
	code := sessionCmd(args)
	stdout, stderr := outDone(), errDone()
	return code, stdout, stderr
}

func writeRecord(t *testing.T, id, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("DUN_HOME"), "sessions", "ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".harness.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionAttribution_Formats(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_SESSION_ID", "")
	writeRecord(t, "s1", `{"version":"v0.4.3","harness":"dun","session_id":"s1","model":"claude-sonnet-5-5","effort":"high"}`)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"s1", "attribution"}, "Agent: dun v0.4.3 (model claude-sonnet-5-5, effort high)\n"},
		{[]string{"s1", "attribution", "--format", "git-trailer"}, "Agent: dun v0.4.3 (model claude-sonnet-5-5, effort high)\n"},
		{[]string{"s1", "attribution", "--format=kv"}, "Agent-Harness: dun\nAgent-Harness-Version: v0.4.3\nAgent-Model: claude-sonnet-5-5\nAgent-Effort: high\n"},
		{[]string{"attribution", "s1", "--format", "json"}, `{"harness":"dun","harness_version":"v0.4.3","model_id":"claude-sonnet-5-5","effort":"high"}` + "\n"},
	} {
		code, out, errOut := runSession(t, c.args...)
		if code != 0 || out != c.want {
			t.Errorf("%v: code %d out %q err %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	// No id: the session a command runs in.
	t.Setenv("DUN_SESSION_ID", "s1")
	if code, out, _ := runSession(t, "attribution"); code != 0 || !strings.HasPrefix(out, "Agent: dun v0.4.3") {
		t.Errorf("env id: %d %q", code, out)
	}
	t.Setenv("DUN_SESSION_ID", "")
	if code, _, errOut := runSession(t, "attribution"); code != 2 || !strings.Contains(errOut, "which session") {
		t.Errorf("no id: %d %q", code, errOut)
	}
	if code, _, errOut := runSession(t, "s1", "attribution", "--format", "xml"); code != 2 || !strings.Contains(errOut, "xml") {
		t.Errorf("bad format: %d %q", code, errOut)
	}
	if code, out, _ := runSession(t, "attribution", "--help"); code != 0 || !strings.Contains(out, "git-trailer") {
		t.Errorf("help: %d %q", code, out)
	}
}

// A dev build's version is the truth; an unknown field is an error naming it.
func TestSessionAttribution_DevAndMissing(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	writeRecord(t, "dev", `{"version":"dev","harness":"dun","model":"m","effort":"default"}`)
	if code, out, _ := runSession(t, "dev", "attribution"); code != 0 || out != "Agent: dun dev (model m, effort default)\n" {
		t.Errorf("dev: %d %q", code, out)
	}
	for field, body := range map[string]string{
		"model_id":        `{"version":"v1","harness":"dun","effort":"low"}`,
		"effort":          `{"version":"v1","harness":"dun","model":"m"}`,
		"harness_version": `{"harness":"dun","model":"m","effort":"low"}`,
		"harness":         `{"version":"v1","model":"m","effort":"low"}`,
	} {
		writeRecord(t, "bad", body)
		for _, f := range []string{"git-trailer", "kv", "json"} {
			code, out, errOut := runSession(t, "bad", "attribution", "--format", f)
			if code != 1 || out != "" || !strings.Contains(errOut, field) {
				t.Errorf("%s/%s: code %d out %q err %q", field, f, code, out, errOut)
			}
		}
	}
}
