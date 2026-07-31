package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func writeTrace(t *testing.T, entries ...traceEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
	f.Close()
	return p
}

func TestReplayProc_EmitsEventsInOrder(t *testing.T) {
	p := writeTrace(t,
		traceEntry{MS: 0, Ev: json.RawMessage(`{"type":"session","id":"s1"}`)},
		traceEntry{MS: 5, Ev: json.RawMessage(`{"type":"ready","tools":["eval"]}`)},
		traceEntry{MS: 10, Ev: json.RawMessage(`{"type":"token","text":"hi"}`)},
	)
	proc, err := replayProc(p, 0) // 0 = as fast as possible
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := 0; i < 4; i++ {
		select {
		case msg := <-proc.ch:
			switch m := msg.(type) {
			case evMsg:
				got = append(got, m["type"].(string))
			case eofMsg:
				got = append(got, "eof")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %v", got)
		}
	}
	want := []string{"session", "ready", "token", "eof"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Pacing is honoured, so a trace reproduces the load that caused a problem
// rather than a firehose of it.
func TestReplayProc_Pacing(t *testing.T) {
	p := writeTrace(t,
		traceEntry{MS: 0, Ev: json.RawMessage(`{"type":"a"}`)},
		traceEntry{MS: 120, Ev: json.RawMessage(`{"type":"b"}`)},
	)
	proc, err := replayProc(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	<-proc.ch
	<-proc.ch
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("events arrived in %v — pacing was not applied", d)
	}
}

// An empty or missing trace fails loudly rather than opening a dead UI.
func TestReplayProc_RejectsEmpty(t *testing.T) {
	if _, err := replayProc(filepath.Join(t.TempDir(), "nope.jsonl"), 1); err == nil {
		t.Error("a missing trace should error")
	}
	if _, err := replayProc(writeTrace(t), 1); err == nil {
		t.Error("an empty trace should error")
	}
}

// The recorder writes what the engine said, with offsets, and never breaks the
// session it is recording.
func TestTraceWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	t.Setenv("DUN_TRACE", path)
	w := newTraceWriter()
	if w == nil {
		t.Fatal("DUN_TRACE set but no writer")
	}
	w.write([]byte(`{"type":"ready"}`))
	time.Sleep(15 * time.Millisecond)
	w.write([]byte(`{"type":"token","text":"x"}`))
	w.close()

	proc, err := replayProc(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := (<-proc.ch).(evMsg)
	if first["type"] != "ready" {
		t.Errorf("recorded the wrong event: %v", first)
	}
	// Writing after close must not panic or resurrect the file.
	w.write([]byte(`{"type":"late"}`))
}

// A finished trace is not a crashed engine: the supervisor must not try to
// respawn one, because there was never a process to respawn.
func TestReplay_DoesNotRestartAnEngine(t *testing.T) {
	m := buildModel(1, 1)
	m.replaying = true
	nm, cmd := m.Update(eofMsg{})
	m = nm.(tuiModel)
	if cmd != nil {
		t.Error("a finished replay must not spawn an engine")
	}
	if m.fatalErr == "" {
		t.Error("the end of a replay should be stated, not silent")
	}
}

var _ tea.Msg = eofMsg{}
