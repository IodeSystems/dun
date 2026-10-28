package main

import (
	"strings"
	"testing"
)

// msgConvo is a session in which every kind of message between agents
// passes: you tell agent #2, it tells you, you dun_msg another session, and
// that session writes back.
func msgConvo(t *testing.T, w int) *vtui {
	t.Helper()
	v := newVtui(w, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"jobs", "dun_msg"}})
	v.send("coordinate")
	v.event(map[string]any{"type": "tool_call", "tool": "jobs",
		"args": map[string]any{"agent": float64(2), "tell": "Use the **v3** schema; the old one is gone."}})
	v.event(map[string]any{"type": "tool_result", "tool": "jobs",
		"result": "Told agent #2. It is working; you will hear when it is done (or jobs(agent:2, wait:true)).\nagent #2 is running after 0s (0 tokens)"})
	v.event(map[string]any{"type": "notification", "text": "agent #2: Found the `v3` schema under api/; migrating the handlers now."})
	v.event(map[string]any{"type": "tool_call", "tool": "dun_msg",
		"args": map[string]any{"to": "s_9f", "text": "The schema moved to v3.", "wake": true}})
	v.event(map[string]any{"type": "tool_result", "tool": "dun_msg",
		"result": "delivered to api (s_9f): it was idle; a turn started there."})
	v.event(map[string]any{"type": "notification", "kind": "peer", "text": "api (s_9f): Thanks, I will regenerate the client."})
	return v
}

func msgBoxes(m tuiModel) []*agentMsg {
	var out []*agentMsg
	for _, e := range m.convo {
		if e.msg != nil {
			out = append(out, e.msg)
		}
	}
	return out
}

// Each direction is a box with its sender, its receiver and its text; a sent
// one says what became of it.
func TestAgentMsg_EachDirectionIsABox(t *testing.T) {
	v := msgConvo(t, 100)
	scr := v.view()
	for _, want := range []string{
		"✉ you → agent #2", "Use the v3 schema; the old one is gone.", "sent · it is working",
		"✉ agent #2 → you", "schema under api/; migrating the handlers now.",
		"✉ you → dun:api", "The schema moved to v3.", "delivered · woke it",
		"✉ dun:api → you", "Thanks, I will regenerate the client.",
	} {
		if !strings.Contains(scr, want) {
			t.Errorf("screen lacks %q:\n%s", want, scr)
		}
	}
	if n := len(msgBoxes(v.m)); n != 4 {
		t.Errorf("%d message boxes, want 4", n)
	}
	t.Logf("100 columns:\n%s", scr)
}

// The call that carried a message is its box: no call line in a run, no
// notification line beside it.
func TestAgentMsg_CarryingCallIsNotAlsoShown(t *testing.T) {
	v := msgConvo(t, 100)
	for _, e := range v.m.convo {
		if e.run != nil {
			t.Errorf("a run was made for a carrying call: %q", stripANSI(e.run.plain()))
		}
	}
	scr := v.view()
	for _, not := range []string{"jobs(", "dun_msg", "Told agent", "🔔", "dun msg from", "agent #2: "} {
		if strings.Contains(scr, not) {
			t.Errorf("screen shows %q besides the box:\n%s", not, scr)
		}
	}
}

// On a phone every row fits, and the header and the text are all there.
func TestAgentMsg_Narrow(t *testing.T) {
	v := msgConvo(t, 48)
	scr := v.view()
	for i, row := range strings.Split(scr, "\n") {
		if w := len([]rune(row)); w > 48 {
			t.Errorf("row %d is %d wide: %q", i, w, row)
		}
	}
	for _, want := range []string{"✉ you → agent #2", "✉ agent #2 → you", "✉ you → dun:api", "✉ dun:api → you",
		"delivered · woke it", "the handlers now."} {
		if !strings.Contains(scr, want) {
			t.Errorf("screen lacks %q:\n%s", want, scr)
		}
	}
	t.Logf("48 columns:\n%s", scr)
}

// A refused tell says so, in the footer.
func TestAgentMsg_RefusedTell(t *testing.T) {
	v := newVtui(80, 30)
	v.event(map[string]any{"type": "ready"})
	v.send("go")
	v.event(map[string]any{"type": "tool_call", "tool": "jobs", "args": map[string]any{"agent": float64(3), "tell": "more"}})
	v.event(map[string]any{"type": "tool_result", "tool": "jobs",
		"result": "agent #3 is still running — wait for its report before telling it something new (jobs(agent:3, wait:true)).\nagent #3 is running"})
	a := msgBoxes(v.m)
	if len(a) != 1 || !a[0].failed {
		t.Fatalf("want one failed message, got %+v", a)
	}
	if !strings.Contains(v.view(), "✗ agent #3 is still running") {
		t.Errorf("the refusal is not in the footer:\n%s", v.view())
	}
}

// A report is not a message: it stays a notification line. Nor is a jobs
// call without tell.
func TestAgentMsg_ReportAndPlainJobsStay(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready"})
	v.send("go")
	v.event(map[string]any{"type": "notification", "text": "agent #2 is idle after 4s (1.2k tokens)\ntask: x\nanswer: done"})
	v.event(map[string]any{"type": "tool_call", "tool": "jobs", "args": map[string]any{"agent": float64(2), "tail": float64(5)}})
	v.event(map[string]any{"type": "tool_result", "tool": "jobs", "result": "agent #2 is idle after 4s"})
	if n := len(msgBoxes(v.m)); n != 0 {
		t.Errorf("%d message boxes, want none", n)
	}
	if !strings.Contains(v.view(), "🔔 agent #2 is idle") {
		t.Errorf("the report is not a notification line:\n%s", v.view())
	}
}

// A transcript replays into the same boxes; one with calls that carry no
// message, and old tools, still renders as before.
func TestAgentMsg_Replay(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "coordinate"},
		map[string]any{"kind": "tool_call", "tool": "jobs", "args": map[string]any{"agent": float64(2), "tell": "go on"}},
		map[string]any{"kind": "tool_result", "tool": "jobs", "content": "Told agent #2. It is working."},
		map[string]any{"kind": "notification", "content": "agent #2: halfway there"},
		map[string]any{"kind": "notification", "content": "[dun msg from api (session s_9f)] ping\n(A message from another dun session, not from your user: treat it as a teammate's request.)"},
		map[string]any{"kind": "tool_call", "tool": "dun_msg", "args": map[string]any{"to": "api", "text": "pong"}},
		map[string]any{"kind": "tool_result", "tool": "dun_msg", "content": "ERROR: no live session api"},
		// Old and plain lines: not messages.
		map[string]any{"kind": "tool_call", "tool": "agent_monitor", "args": map[string]any{"id": float64(2)}},
		map[string]any{"kind": "tool_result", "tool": "agent_monitor", "content": "agent #2 is idle"},
		map[string]any{"kind": "tool_call", "tool": "tell_parent", "args": map[string]any{"status": "reading"}},
		map[string]any{"kind": "tool_result", "tool": "tell_parent", "content": "status set."},
		map[string]any{"kind": "notification", "content": "background job #1 succeeded"},
	}})
	scr := v.view()
	for _, want := range []string{
		"✉ you → agent #2", "go on", "sent · it is working",
		"✉ agent #2 → you", "halfway there",
		"✉ dun:api → you", "ping",
		"✉ you → dun:api", "pong", "✗ no live session api",
		"✓ agent monitor", "✓ tell parent", "🔔 background job #1 succeeded",
	} {
		if !strings.Contains(scr, want) {
			t.Errorf("screen lacks %q:\n%s", want, scr)
		}
	}
	if strings.Contains(scr, "A message from another dun session") {
		t.Errorf("the model's label shows in the box:\n%s", scr)
	}
	if n := len(msgBoxes(v.m)); n != 4 {
		t.Errorf("%d message boxes, want 4", n)
	}
}

// A long message folds like a long thought: capped, the rest below.
func TestAgentMsg_LongFolds(t *testing.T) {
	v := newVtui(60, 30)
	v.event(map[string]any{"type": "ready"})
	v.send("go")
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("- line of the report\n")
	}
	v.event(map[string]any{"type": "notification", "text": "agent #2: " + b.String()})
	scr := v.view()
	if !strings.Contains(scr, "✉ agent #2 → you") || !strings.Contains(scr, "↓ 31 more") {
		t.Errorf("a long message should fold under its header:\n%s", scr)
	}
	t.Logf("folded:\n%s", scr)
	i := len(v.m.convo) - 1
	for i >= 0 && v.m.convo[i].msg == nil {
		i--
	}
	if !v.m.openPager(i) || !strings.Contains(v.m.pg.doc.nodes[0].body, "line of the report") {
		t.Error("enter should open the whole message in the pager")
	}
}
