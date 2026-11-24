package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// clearRunner answers "ok" to every call and records each request. With
// exec, its first call runs `sleep 30` with a 1s grace instead: a
// background job #1 that outlives the turn. With ack, the next call
// acknowledges the person's message as an open ask (the asks tool), so the
// message is an ask and not unacknowledged intake.
type clearRunner struct {
	exec bool
	ack  bool
	mu   sync.Mutex
	n    int
	reqs [][]llm.Message
}

func (r *clearRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.n++
	var name, args string
	switch n := r.n; {
	case r.exec && n == 1:
		name, args = "exec", `{"command":"sleep 30","timeout":1}`
	case r.ack && n == map[bool]int{true: 2, false: 1}[r.exec]:
		name, args = "asks", `{"asks":[{"id":0,"state":"open"}]}`
	}
	r.reqs = append(r.reqs, msgs)
	r.mu.Unlock()
	out := make(chan llm.StreamChunk, 4)
	if name != "" {
		tc := &llm.ToolCall{ID: "c" + strconv.Itoa(r.n), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = name, args
		out <- llm.StreamChunk{ToolCall: tc}
	} else {
		out <- llm.StreamChunk{Content: "ok"}
	}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	close(out)
	return out, nil
}

// last is the newest request's text, system prompt left out.
func (r *clearRunner) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if len(r.reqs) > 0 {
		for _, m := range r.reqs[len(r.reqs)-1] {
			if m.Role != "system" {
				b.WriteString(m.Role + ": " + m.Content + "\n")
			}
		}
	}
	return b.String()
}

// clearRig is an engine whose first turn ran: with exec, job #1 runs on
// after it.
func clearRig(t *testing.T, exec bool) (*engineRig, *clearRunner) {
	t.Helper()
	return clearRigAck(t, exec, true)
}

// clearRigAck is clearRig with the first message acknowledged as an ask
// (ack) or left unacknowledged on the intake.
func clearRigAck(t *testing.T, exec, ack bool) (*engineRig, *clearRunner) {
	t.Helper()
	r := &clearRunner{exec: exec, ack: ack}
	g := newEngineRig(t, r, func(c *harness.Config) { c.Exec = &harness.HostShell{Dir: c.Workspace} })
	t.Cleanup(func() { g.h.KillAllJobs("user") })
	g.waitFor("ready", isType("ready"))
	g.send(&proto.User{Content: "the old task"})
	g.waitFor("the turn's done", isType("done"))
	if exec && len(g.h.RunningJobs()) != 1 {
		t.Fatalf("job #1 should run on after the turn: %+v", g.h.RunningJobs())
	}
	return g, r
}

func (g *engineRig) slash(line string) { g.send(&proto.ControlCmd{ID: "slash", Action: line}) }

// noReset fails if a reset comes within a moment.
func (g *engineRig) noReset(why string) {
	g.t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-g.events:
			g.seen = append(g.seen, ev)
			if ev["type"] == "reset" {
				g.t.Fatalf("%s: the conversation was cleared", why)
			}
		case <-deadline:
			return
		}
	}
}

// Only open asks (the old task, acknowledged): a bare /clear asks about
// the plan alone, clears nothing, and the message names the line to send.
// Sending nothing is the cancel: the items are still there.
func TestEngineClearAsksAboutPlanOnly(t *testing.T) {
	g, _ := clearRig(t, false)
	g.slash("clear")
	q := g.waitFor("the question", isType("clear_confirm"))
	if q["jobs"] != nil {
		t.Errorf("no job runs, yet jobs = %v", q["jobs"])
	}
	plan, _ := q["plan"].([]any)
	if len(plan) != 1 || plan[0].(map[string]any)["kind"] != "ask" || !strings.Contains(plan[0].(map[string]any)["title"].(string), "the old task") {
		t.Errorf("plan = %v, want the ask for the old task", q["plan"])
	}
	msg, _ := q["message"].(string)
	if !strings.Contains(msg, "/clear plan=keep|drop") || strings.Contains(msg, "jobs=") {
		t.Errorf("message = %q", msg)
	}
	g.noReset("the question")
	if len(g.h.ClearItems()) != 1 {
		t.Errorf("unanswered, the items must stay: %+v", g.h.ClearItems())
	}
}

// Jobs and plan: a bare /clear asks about both; a line that answers one
// asks about the other alone; keep for both clears, the job runs on, and
// the new conversation's first request says what was kept.
func TestEngineClearKeepTellsTheNewConversation(t *testing.T) {
	g, r := clearRig(t, true)
	g.slash("clear")
	q := g.waitFor("the question", isType("clear_confirm"))
	jobs, _ := q["jobs"].([]any)
	plan, _ := q["plan"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["command"] != "sleep 30" || len(plan) != 1 {
		t.Fatalf("both asked: jobs %v plan %v", q["jobs"], q["plan"])
	}
	g.slash("clear plan=keep")
	q = g.waitFor("the jobs question", isType("clear_confirm"))
	if q["plan"] != nil || q["jobs"] == nil {
		t.Errorf("plan answered: jobs %v plan %v", q["jobs"], q["plan"])
	}
	g.noReset("half an answer")

	g.slash("clear jobs=keep plan=keep")
	g.waitFor("reset", isType("reset"))
	if len(g.h.RunningJobs()) != 1 {
		t.Fatal("a kept job must run on")
	}
	g.send(&proto.User{Content: "the new task"})
	g.waitFor("the new turn's done", isType("done"))
	got := r.last()
	for _, want := range []string{"kept these from before it", "#1 `sleep 30`", "ask 1 ", "the old task", "user: the new task"} {
		if !strings.Contains(got, want) {
			t.Errorf("the new conversation's request lacks %q:\n%s", want, got)
		}
	}
}

// Stop and drop: the job is killed without a completion notice, the asks
// are dropped, and the new conversation hears of neither.
func TestEngineClearStopAndDrop(t *testing.T) {
	g, r := clearRig(t, true)
	g.slash("clear drop")
	g.waitFor("reset", isType("reset"))
	deadline := time.Now().Add(5 * time.Second)
	for len(g.h.RunningJobs()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("a stopped job still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if items := g.h.ClearItems(); len(items) != 0 {
		t.Errorf("dropped items remain: %+v", items)
	}
	time.Sleep(100 * time.Millisecond) // the kill's notice, if one were sent
	g.send(&proto.User{Content: "the new task"})
	g.waitFor("the new turn's done", isType("done"))
	got := r.last()
	for _, gone := range []string{"background job #1", "sleep 30", "the old task", "kept these"} {
		if strings.Contains(got, gone) {
			t.Errorf("the new conversation's request has %q:\n%s", gone, got)
		}
	}
}

// A /clear argument that is not one is answered with the usage, and
// nothing is cleared.
func TestEngineClearBadArg(t *testing.T) {
	g, _ := clearRig(t, false)
	g.slash("clear everything")
	c := g.waitFor("the usage", isType("control"))
	if msg, _ := c["message"].(string); !strings.HasPrefix(msg, clearUsage) {
		t.Errorf("reply = %q", msg)
	}
	g.noReset("a bad argument")
}

func TestParseClear(t *testing.T) {
	for in, want := range map[string]clearChoice{
		"":                    {},
		"keep":                {"keep", "keep"},
		"drop":                {"stop", "drop"},
		"stop":                {"stop", "drop"},
		"jobs=keep":           {jobs: "keep"},
		"plan=drop":           {plan: "drop"},
		"jobs=stop plan=keep": {"stop", "keep"},
		"Jobs=Drop":           {jobs: "stop"},
		"keep plan=drop":      {"keep", "drop"},
	} {
		got, err := parseClear(strings.Fields(in))
		if err != nil || got != want {
			t.Errorf("parseClear(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"all", "jobs=", "plan=maybe", "agents=keep"} {
		if _, err := parseClear([]string{bad}); err == nil {
			t.Errorf("parseClear(%q) took it", bad)
		}
	}
}

// Only an unacknowledged message (the model never acked it): a bare /clear
// asks nothing and clears at once; the message is gone from the new
// conversation (USER 2026-11-24).
func TestEngineClearDropsUnacknowledgedWithoutAsking(t *testing.T) {
	g, r := clearRigAck(t, false, false)
	if len(g.h.ClearItems()) != 0 {
		t.Fatalf("an unacknowledged message is not an item: %+v", g.h.ClearItems())
	}
	g.slash("clear")
	g.waitFor("reset", isType("reset"))
	for _, ev := range g.seen {
		if ev["type"] == "clear_confirm" {
			t.Fatal("asked about an unacknowledged message")
		}
	}
	g.send(&proto.User{Content: "the new task"})
	g.waitFor("the new turn's done", isType("done"))
	got := r.last()
	if strings.Contains(got, "the old task") || strings.Contains(got, "kept these") {
		t.Errorf("the new conversation's request has the unacknowledged message:\n%s", got)
	}
}

// An acknowledged ask and a later unacknowledged message: the question
// lists the ask alone; keep carries the ask, not the message.
func TestEngineClearKeepCarriesAskNotUnacknowledged(t *testing.T) {
	g, r := clearRig(t, false)
	g.send(&proto.User{Content: "the unacked one"})
	g.waitFor("the second turn's done", isType("done"))
	g.slash("clear")
	q := g.waitFor("the question", isType("clear_confirm"))
	plan, _ := q["plan"].([]any)
	if len(plan) != 1 || plan[0].(map[string]any)["kind"] != "ask" || strings.Contains(plan[0].(map[string]any)["title"].(string), "unacked") {
		t.Fatalf("plan = %v, want the one ask", q["plan"])
	}
	g.slash("clear keep")
	g.waitFor("reset", isType("reset"))
	g.send(&proto.User{Content: "the new task"})
	g.waitFor("the new turn's done", isType("done"))
	got := r.last()
	if !strings.Contains(got, "ask 1 ") || strings.Contains(got, "unacked") {
		t.Errorf("the new conversation's request:\n%s", got)
	}
}
