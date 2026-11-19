package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
)

// errorsHome is a $DUN_HOME with two sessions' errors files.
func errorsHome(t *testing.T) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := filepath.Join(harness.SessionsDir(), "-src-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	write := func(session string, recs ...harness.TurnError) {
		var b bytes.Buffer
		for _, r := range recs {
			r.Session = session
			line, _ := json.Marshal(r)
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(filepath.Join(dir, session+".errors.jsonl"), b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("20261118-190000",
		harness.TurnError{Time: at, Provider: "iode", Host: "llm.iodesystems.com", Model: "qwen", Error: "agent: chat: JSON error injected into SSE stream"},
		harness.TurnError{Time: at.Add(2 * time.Minute), Provider: "iode", Host: "llm.iodesystems.com", Model: "qwen", Status: 502,
			Error: "agent: chat: llm: status 502: bad gateway", Payload: "bad gateway\nupstream reset"})
	write("20261118-195000",
		harness.TurnError{Time: at.Add(time.Minute), Provider: "or", Host: "openrouter.ai", Model: "m", Error: "boom"})
}

func TestErrorsCmd_Text(t *testing.T) {
	errorsHome(t)
	var out, errw bytes.Buffer
	if code := errorsCmd(nil, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	want := `2026-11-18T20:00:00Z · 20261118-190000 · iode · llm.iodesystems.com · qwen
  agent: chat: JSON error injected into SSE stream

2026-11-18T20:01:00Z · 20261118-195000 · or · openrouter.ai · m
  boom

2026-11-18T20:02:00Z · 20261118-190000 · iode · llm.iodesystems.com · qwen · status 502
  agent: chat: llm: status 502: bad gateway
  payload: bad gateway
    upstream reset
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	if code := errorsCmd([]string{"--session", "20261118-195000"}, &out, &errw); code != 0 || out.String() != "2026-11-18T20:01:00Z · 20261118-195000 · or · openrouter.ai · m\n  boom\n" {
		t.Errorf("--session: exit %d:\n%s", code, out.String())
	}
	out.Reset()
	if code := errorsCmd([]string{"--session=nope"}, &out, &errw); code != 0 || !bytes.Contains(out.Bytes(), []byte("no turn errors kept in session nope")) {
		t.Errorf("unknown session: exit %d: %s", code, out.String())
	}
	if code := errorsCmd([]string{"-n", "x"}, &out, &errw); code != 2 {
		t.Errorf("-n x: exit %d", code)
	}
}

func TestErrorsCmd_JSON(t *testing.T) {
	errorsHome(t)
	var out, errw bytes.Buffer
	if code := errorsCmd([]string{"--json", "-n", "2"}, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	var recs []harness.TurnError
	if err := json.Unmarshal(out.Bytes(), &recs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if len(recs) != 2 || recs[0].Session != "20261118-195000" || recs[1].Status != 502 || recs[1].Payload != "bad gateway\nupstream reset" {
		t.Errorf("records = %+v", recs)
	}
	t.Setenv("DUN_HOME", t.TempDir())
	out.Reset()
	if code := errorsCmd([]string{"--json"}, &out, &errw); code != 0 || out.String() != "[]\n" {
		t.Errorf("none: exit %d %q", code, out.String())
	}
}
