package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// After a fold, the next redundant cd carries the line again; a fold alone
// says nothing, and a cd that is not redundant still gets none.
func TestForget_FoldResetsCdNudge(t *testing.T) {
	h := newNoteHarness(t)
	dir := t.TempDir()
	inner := func(context.Context, llm.ToolCall) (string, error) { return "ok", nil }
	d := withCdNudge(inner, dir, &h.cdNudges)
	call := func(cmd string) bool {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = "exec", string(args)
		out, _ := d(context.Background(), tc)
		return strings.Contains(out, "leading cd is not needed")
	}
	redundant := "cd " + dir + " && ls"
	if !call(redundant) || call(redundant) {
		t.Fatal("want the line on the 1st redundant cd only")
	}
	h.noteCompactionCause(agent.CompactionInfo{SubsumedCount: 4}, foldByShaper)
	if call("cd sub && ls") {
		t.Error("a cd that is not redundant must not carry the line after a fold")
	}
	if !call(redundant) {
		t.Error("the 1st redundant cd after a fold must carry the line again")
	}
	if call(redundant) {
		t.Error("the 2nd redundant cd after a fold must not")
	}
}

// A recap cue can be raised again after a fold and after a recap. A
// large-result cue stays claimed: its result and aside go or stay together.
func TestForget_FoldAndRecapForgetRecapCues(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{store: st, cfg: Config{SessionFile: path}, wake: make(chan struct{}, 1)}
	const cue = "repeat:exec:3"
	big := bigCueKey("c1")
	if !h.claimCue(cue) || h.claimCue(cue) || !h.claimCue(big) {
		t.Fatal("claimCue: want first claim true, second false")
	}
	h.noteCompactionCause(agent.CompactionInfo{SubsumedCount: 4}, foldByBoundary)
	if !h.claimCue(cue) {
		t.Error("after a fold the cue must be claimable again")
	}
	if h.claimCue(big) {
		t.Error("a large-result cue must stay claimed after a fold")
	}

	for _, e := range convo() {
		st.appendSilent(e)
	}
	entries, _ := st.Context(context.Background(), "dun")
	sp, err := planRecap(entries, "nested quotes", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.applyRecap(sp, "done"); err != nil {
		t.Fatal(err)
	}
	if !h.claimCue(cue) {
		t.Error("after a recap the cue must be claimable again")
	}
}

// What the person sees once per session, and the learn cost cap, stay set.
func TestForget_KeepsUserFacingOnce(t *testing.T) {
	h := newNoteHarness(t)
	h.setupSaid = map[string]bool{"lsp": true}
	h.indexAsked = map[string]bool{"lsp": true}
	h.learn.calls = 2
	h.noteCompactionCause(agent.CompactionInfo{SubsumedCount: 4}, foldByOverflow)
	if !h.setupSaid["lsp"] || !h.indexAsked["lsp"] || h.learn.calls != 2 {
		t.Errorf("a fold reset user-facing state: setupSaid=%v indexAsked=%v learn.calls=%d",
			h.setupSaid, h.indexAsked, h.learn.calls)
	}
}
