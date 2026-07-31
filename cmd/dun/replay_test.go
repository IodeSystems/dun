package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	proc, err := replayProc(p, replayPacing{delay: 0}) // 0 = as fast as possible
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
	proc, err := replayProc(p, replayPacing{speed: 1, delay: -1})
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
	if _, err := replayProc(filepath.Join(t.TempDir(), "nope.jsonl"), replayPacing{delay: 0}); err == nil {
		t.Error("a missing trace should error")
	}
	if _, err := replayProc(writeTrace(t), replayPacing{delay: 0}); err == nil {
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

	proc, err := replayProc(path, replayPacing{delay: 0})
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

// Pacing is the point of a trace: the same 60 tokens in a second and spread
// over a minute are different loads, and only one reproduces a stutter. But
// CI does not want to sit through a ten-minute session, so the recording can be
// overridden — including to no delay at all.
func TestReplayPacing(t *testing.T) {
	cases := []struct {
		name    string
		pacing  replayPacing
		ms      int64
		elapsed time.Duration
		want    time.Duration
	}{
		{"recorded timing", replayPacing{speed: 1, delay: -1}, 100, 0, 100 * time.Millisecond},
		{"recorded, already late", replayPacing{speed: 1, delay: -1}, 100, 120 * time.Millisecond, -20 * time.Millisecond},
		{"double speed", replayPacing{speed: 2, delay: -1}, 100, 0, 50 * time.Millisecond},
		{"fast-forward", replayPacing{speed: 1, delay: 0}, 100000, 0, 0},
		{"fixed gap overrides the recording", replayPacing{speed: 1, delay: 5 * time.Millisecond}, 100000, 0, 5 * time.Millisecond},
	}
	for _, c := range cases {
		if got := c.pacing.gap(c.ms, c.elapsed); got != c.want {
			t.Errorf("%s: gap = %v, want %v", c.name, got, c.want)
		}
	}
}

// A trace records WHEN, not just what — without offsets it could not reproduce
// a load at all.
func TestTrace_RecordsOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	t.Setenv("DUN_TRACE", path)
	w := newTraceWriter()
	w.write([]byte(`{"type":"a"}`))
	time.Sleep(30 * time.Millisecond)
	w.write([]byte(`{"type":"b"}`))
	w.close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []traceEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e traceEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[1].MS < 20 {
		t.Errorf("second event recorded at %dms — the gap was not captured", got[1].MS)
	}
}

// Only the SHORT gaps carry load. Tokens 16ms apart are what stutters; the four
// minutes while someone read the reply reproduce nothing but four minutes. So
// bursts must survive exactly and dead air must not.
func TestSqueezeIdle(t *testing.T) {
	// A burst, a long think, another burst.
	entries := []traceEntry{
		{MS: 0}, {MS: 16}, {MS: 32}, // burst: 16ms apart
		{MS: 240032},               // 4 minutes of nothing
		{MS: 240048}, {MS: 240064}, // burst again
	}
	st := squeezeIdle(entries, replayPacing{speed: 1, delay: -1, maxGap: 2 * time.Second})

	if st.Squeezed != 1 {
		t.Errorf("want 1 compressed gap, got %d", st.Squeezed)
	}
	if st.Span < 4*time.Minute {
		t.Errorf("span should be the RECORDING's length, got %s", st.Span)
	}
	if st.Played > 3*time.Second {
		t.Errorf("a 4-minute session should replay in seconds, got %s", st.Played)
	}
	// Every burst gap is untouched — this is the part that reproduces load.
	for _, want := range []struct {
		i   int
		gap int64
	}{{1, 16}, {2, 16}, {4, 16}, {5, 16}} {
		if got := entries[want.i].MS - entries[want.i-1].MS; got != want.gap {
			t.Errorf("burst gap at %d = %dms, want %dms — bursts must be verbatim", want.i, got, want.gap)
		}
	}
	// The idle gap became exactly the cap.
	if got := entries[3].MS - entries[2].MS; got != 2000 {
		t.Errorf("idle gap = %dms, want the 2000ms cap", got)
	}
	// Offsets stay absolute and monotonic, or the player's jitter-proof
	// sleep-to-offset scheduling breaks.
	for i := 1; i < len(entries); i++ {
		if entries[i].MS < entries[i-1].MS {
			t.Fatalf("offsets are no longer monotonic at %d", i)
		}
	}
}

// maxGap 0 means verbatim: some investigations want the real wall clock.
func TestSqueezeIdle_VerbatimWhenDisabled(t *testing.T) {
	entries := []traceEntry{{MS: 0}, {MS: 600000}}
	st := squeezeIdle(entries, replayPacing{speed: 1, delay: -1, maxGap: 0})
	if st.Squeezed != 0 || entries[1].MS != 600000 {
		t.Errorf("maxGap 0 must not rewrite the recording: squeezed=%d ms=%d", st.Squeezed, entries[1].MS)
	}
}

// A replay that rewrites time has to SAY so — otherwise it is evidence you
// cannot trust.
func TestReplayStats_ReportsWhatItChanged(t *testing.T) {
	st := &replayStats{Events: 40, Span: 5 * time.Minute, Played: 6 * time.Second, Squeezed: 3}
	s := st.String()
	for _, want := range []string{"40 events", "5m0s", "3 idle gap", "6s"} {
		if !strings.Contains(s, want) {
			t.Errorf("stats missing %q: %s", want, s)
		}
	}
	// Nothing compressed → nothing to explain.
	if s := (&replayStats{Events: 2, Span: time.Second}).String(); strings.Contains(s, "compressed") {
		t.Errorf("no compression should not be announced: %s", s)
	}
}
