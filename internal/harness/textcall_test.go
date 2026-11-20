package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// dsmlSample is the DeepSeek Flash reply (USER 2026-11-18): a tool call as
// DSML text with fullwidth bars, mismatched closers, no tool_calls.
const dsmlSample = "Let me check the build.\n\n" +
	"<｜DSML｜function_calls>\n" +
	"<｜DSML｜invoke name=\"exec\">\n" +
	"<｜DSML｜parameter name=\"command\" string=\"true\">go build ./...</｜DSML｜parameter>\n" +
	"</｜DSML｜function_calls>"

// textCallHarness starts a session whose model answers text(i) to request i.
func textCallHarness(t *testing.T, text func(int) string) (*Harness, *nudgeRunner) {
	t.Helper()
	dir := t.TempDir()
	r := &nudgeRunner{text: text}
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile:     filepath.Join(dir, "s.jsonl"),
		PlanNudgeRounds: -1, PlanNudgeFails: -1,
		NoAsks: true, // no ask node, so no turn-end check: the requests counted are the corrections
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, r
}

// The DSML sample is corrected and the turn goes on; a model that keeps
// doing it gets textCallsMax corrections, then the turn ends. A new turn has
// its corrections again.
func TestTextCall_RetriesThenStopsAtCap(t *testing.T) {
	h, r := textCallHarness(t, func(int) string { return dsmlSample })
	if _, err := h.Ask(context.Background(), "build it"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 1+textCallsMax {
		t.Fatalf("%d requests, want %d", n, 1+textCallsMax)
	}
	note := "[automatic] your last reply had a tool call written as text (DSML markup); it did not run."
	if r.sentHas(0, note) || !r.sentHas(1, note) {
		t.Errorf("correction missing from request 1, or present in request 0")
	}
	if _, err := h.Ask(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 2*(1+textCallsMax) {
		t.Fatalf("%d requests after the second turn, want %d", n, 2*(1+textCallsMax))
	}
}

// A wake whose reply is empty reads no earlier turn's reply: the last
// turn's DSML is not corrected again (review 2026-11-18). Its two requests
// are the empty round and agentkit's one empty-reply repair (v0.38.0).
func TestTextCall_OldReplyNotReread(t *testing.T) {
	h, r := textCallHarness(t, func(i int) string {
		if i <= textCallsMax {
			return dsmlSample
		}
		return ""
	})
	if _, err := h.Ask(context.Background(), "build it"); err != nil {
		t.Fatal(err)
	}
	if err := wakeTurn(h, "job 1 finished"); !errors.Is(err, agent.ErrEmptyReply) {
		t.Fatalf("wake: %v, want an empty reply", err)
	}
	if n, want := r.calls(), 1+textCallsMax+2; n != want {
		t.Fatalf("%d requests, want %d: the wake was corrected for the last turn's reply", n, want)
	}
}

// One correction, then a proper reply: the turn ends there.
func TestTextCall_OnceThenReply(t *testing.T) {
	h, r := textCallHarness(t, func(i int) string {
		if i == 0 {
			return dsmlSample
		}
		return "done"
	})
	if _, err := h.Ask(context.Background(), "build it"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

// Markup in a code fence or inline code is prose about the tags: no
// correction.
func TestTextCall_CodeIgnored(t *testing.T) {
	reply := "DeepSeek writes calls like this:\n\n```\n" + dsmlSample + "\n```\n\n" +
		"and others use `<invoke name=\"x\">` with `<parameter>` or `<tool_call>`."
	h, r := textCallHarness(t, func(int) string { return reply })
	if _, err := h.Ask(context.Background(), "explain"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// A plan argument with the markup is refused with the guidance and nothing
// is added; a tag named in prose or in inline code is accepted.
func TestTextCall_PlanArgRefused(t *testing.T) {
	h := planHarness(t, false)
	d := withPlan(nil, h.planTools(), nil)
	args := `{"title":"build","details":` + strconv.Quote(dsmlSample) + `}`
	out, err := d(context.Background(), *nudgeCall("p1", "plan_add", args))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "ERROR: plan_add refused") || !strings.Contains(out, "tool API") {
		t.Errorf("not refused: %s", out)
	}
	if g, _ := (planStore{h}).View(context.Background()); len(g.Nodes) != 0 {
		t.Errorf("node added: %+v", g.Nodes)
	}
	for _, ok := range []string{
		`{"title":"document ` + "`<tool_call>`" + ` tags","done_when":"written"}`,
		`{"title":"parser","details":"fix <tool_call> parsing","done_when":"tests pass"}`,
	} {
		if out, _ := d(context.Background(), *nudgeCall("p2", "plan_add", ok)); strings.HasPrefix(out, "ERROR") {
			t.Errorf("a tag named in text refused: %s", out)
		}
	}
}
