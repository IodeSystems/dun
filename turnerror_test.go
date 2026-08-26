package dun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// The whole point: a failed turn survives the scrollback. Before this the
// session log showed a tool call with no result and no statement of why.
func TestRecordTurnError_PersistsAndSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{store: s}
	h.RecordTurnError(errors.New("the turn ran past its 30m0s budget (--timeout) and was cut off"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--timeout") {
		t.Fatalf("the reason never reached disk: %s", raw)
	}

	re, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if re.Loaded() != 1 {
		t.Fatalf("want the error entry reloaded, got %d", re.Loaded())
	}
}

// It is a note to a reader, not conversation: the model must never be handed
// its own harness's failure to explain.
func TestRecordTurnError_NeverReachesTheModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	ctx := context.Background()
	s, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(ctx, "x", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "hello"})
	h := &Harness{store: s}
	h.RecordTurnError(errors.New("context canceled"))
	s.Append(ctx, "x", agent.Entry{ID: "2", Kind: agent.KindUser, Content: "still here"})

	got, err := s.Context(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want only the 2 user messages in context, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Kind == kindTurnError {
			t.Fatal("a turn error was handed to the model")
		}
	}

	// And the filter holds across a resume, not just in the writing process.
	re, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = re.Context(ctx, "x"); len(got) != 2 {
		t.Fatalf("after reload, want 2 entries in context, got %d: %+v", len(got), got)
	}
}

// Recording must never fail a turn harder than it already failed.
func TestRecordTurnError_NilSafe(t *testing.T) {
	var h *Harness
	h.RecordTurnError(errors.New("boom")) // no panic

	s, err := openSessionStore(filepath.Join(t.TempDir(), "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	live := &Harness{store: s}
	live.RecordTurnError(nil)
	if got, _ := s.Context(context.Background(), "x"); len(got) != 0 {
		t.Fatalf("a nil error was recorded: %+v", got)
	}
}
