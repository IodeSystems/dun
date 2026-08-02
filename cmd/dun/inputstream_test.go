package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drain reads users to completion so the scanner goroutine can finish and close
// the channels, then reports the messages it saw.
func drain(t *testing.T, s *inputStream) []string {
	t.Helper()
	var got []string
	for {
		select {
		case c, ok := <-s.users:
			if !ok {
				return got
			}
			got = append(got, c)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the input stream to close")
		}
	}
}

// TestInputStreamEOFIsNotAStop pins the distinction runProgrammatic relies on:
// plain EOF must NOT look like an explicit stop, or a one-shot `dun -p 'task'`
// exits with its background jobs still in flight and never delivers their
// completion notifications.
func TestInputStreamEOFIsNotAStop(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(`{"type":"user","content":"hi"}` + "\n"))
	if got := drain(t, s); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("messages = %q, want [hi]", got)
	}
	if s.stopped() {
		t.Fatal("stopped() = true after plain EOF, want false (EOF must drain, not bail)")
	}
}

func TestInputStreamExplicitStop(t *testing.T) {
	for _, kind := range []string{"stop", "quit"} {
		t.Run(kind, func(t *testing.T) {
			s := newInputStreamFrom(strings.NewReader(
				`{"type":"user","content":"hi"}` + "\n" + `{"type":"` + kind + `"}` + "\n"))
			if got := drain(t, s); len(got) != 1 || got[0] != "hi" {
				t.Fatalf("messages = %q, want [hi]", got)
			}
			if !s.stopped() {
				t.Fatalf("stopped() = false after an explicit %s, want true", kind)
			}
		})
	}
}

// TestInputStreamStopEndsReading: input after a stop is ignored — the scanner
// returns rather than blocking on a send nobody will receive.
func TestInputStreamStopEndsReading(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(
		`{"type":"stop"}` + "\n" + `{"type":"user","content":"after"}` + "\n"))
	if got := drain(t, s); len(got) != 0 {
		t.Fatalf("messages = %q, want none after a stop", got)
	}
	if !s.stopped() {
		t.Fatal("stopped() = false, want true")
	}
}

func TestInputStreamRoutesAnswers(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(
		`{"type":"answer","value":"yes"}` + "\n" + `{"type":"user","content":"hi"}` + "\n"))
	select {
	case v := <-s.answers:
		if v != "yes" {
			t.Fatalf("answer = %q, want yes", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the answer event")
	}
	if got := drain(t, s); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("messages = %q, want [hi]", got)
	}
}

// TestInputStreamSkipsJunk: blank lines and non-JSON are ignored rather than
// ending the stream, so a noisy pipe doesn't look like EOF.
func TestInputStreamSkipsJunk(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(
		"\n" + "not json\n" + `{"type":"unknown"}` + "\n" + `{"type":"user","content":"hi"}` + "\n"))
	if got := drain(t, s); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("messages = %q, want [hi]", got)
	}
	if s.stopped() {
		t.Fatal("stopped() = true, want false")
	}
}

// TestInputStreamResetCallback verifies that a "reset" event invokes the
// callback installed via setResetCb.
func TestInputStreamResetCallback(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(
		`{"type":"reset"}` + "\n" +
			`{"type":"user","content":"after"}` + "\n"))
	var called atomic.Bool
	s.setResetCb(func() { called.Store(true) })
	// Give the scanner a moment to process the reset event.
	time.Sleep(50 * time.Millisecond)
	if !called.Load() {
		t.Fatal("reset callback not invoked")
	}
	if got := drain(t, s); len(got) != 1 || got[0] != "after" {
		t.Fatalf("messages = %q, want [after]", got)
	}
}
