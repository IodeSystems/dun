package acp

import (
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

func untilSetAsync(t *testing.T, r *rig, sid sdk.SessionId, id, value string) chan sdk.SetSessionConfigOptionResponse {
	ch := make(chan sdk.SetSessionConfigOptionResponse, 1)
	go func() {
		out, err := set(r, sid, id, value)
		if err != nil {
			t.Error(err)
		}
		ch <- out
	}()
	return ch
}

func answered(t *testing.T, ch chan sdk.SetSessionConfigOptionResponse) []sdk.SessionConfigOption {
	t.Helper()
	select {
	case out := <-ch:
		return out.ConfigOptions
	case <-time.After(5 * time.Second):
		t.Fatal("set_config_option not answered")
	}
	return nil
}

func untilCmdSent(t *testing.T, r *rig, want string) {
	t.Helper()
	c := r.d.next()
	if cc, ok := c.Payload.(*proto.ControlCmd); !ok || cc.ID != "until" || cc.Action != want {
		t.Fatalf("sent %s %+v, want until %q", c.Type, c.Payload, want)
	}
}

// mode and turn_cap are listed, off; plan sends /until plan and answers
// once the engine reports it; a turn cap with mode off is "run N turns";
// the reply to the bridge's own line is not shown.
func TestUntilOptions(t *testing.T) {
	r, res := configRig(t)
	if current(res.ConfigOptions, optMode) != "off" || current(res.ConfigOptions, optTurnCap) != "off" {
		t.Fatalf("options %+v", res.ConfigOptions)
	}
	for _, o := range res.ConfigOptions {
		if o.Select != nil && o.Select.Id == optMode && *o.Select.Category != sdk.SessionConfigOptionCategoryMode {
			t.Errorf("mode category %v", *o.Select.Category)
		}
	}
	ch := untilSetAsync(t, r, res.SessionId, optMode, "plan")
	untilCmdSent(t, r, "plan")
	r.d.emit(&proto.ControlEvent{ID: "until", Message: "until: plan"},
		&proto.Until{Conds: []proto.UntilCond{{Kind: "plan"}}, Cap: 20, Line: "until plan"})
	if opts := answered(t, ch); current(opts, optMode) != "plan" {
		t.Fatalf("answer %+v", opts)
	}
	ch = untilSetAsync(t, r, res.SessionId, optTurnCap, "10")
	untilCmdSent(t, r, "plan turns 10")
	r.d.emit(&proto.Until{Conds: []proto.UntilCond{{Kind: "plan"}, {Kind: "turns", N: 10}}, Cap: 10})
	if opts := answered(t, ch); current(opts, optTurnCap) != "10" {
		t.Fatalf("answer %+v", opts)
	}
	ch = untilSetAsync(t, r, res.SessionId, optMode, "off")
	untilCmdSent(t, r, "turns 10")
	r.d.emit(&proto.Until{Conds: []proto.UntilCond{{Kind: "turns", N: 10}}, Cap: 10})
	if opts := answered(t, ch); current(opts, optMode) != "off" || current(opts, optTurnCap) != "10" {
		t.Fatalf("answer %+v", opts)
	}
	for _, u := range r.ed.take() {
		if u.AgentMessageChunk != nil {
			t.Errorf("shown: %+v", u.AgentMessageChunk)
		}
	}
	// A cap set elsewhere (the TUI's /until turns 15) is listed and current.
	r.d.emit(&proto.Until{Conds: []proto.UntilCond{{Kind: "turns", N: 15}}, Cap: 15})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, u := range configUpdates(r) {
			if current(u, optTurnCap) == "15" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no config_option_update for a cap set elsewhere")
}

// goal takes its text from the next prompt: nothing is sent at the set,
// and the prompt sends /until goal "<text>" before itself. The prompt is
// answered when the run stops, not at its first turn's end.
func TestUntilGoalFromPrompt(t *testing.T) {
	r, res := configRig(t)
	sid := res.SessionId
	opts := answered(t, untilSetAsync(t, r, sid, optMode, "goal"))
	if current(opts, optMode) != "goal" {
		t.Fatalf("answer %+v", opts)
	}
	select {
	case c := <-r.d.cmds:
		t.Fatalf("sent %s at the set", c.Type)
	case <-time.After(100 * time.Millisecond):
	}
	ch := r.prompt(sid, `make "it" pass`)
	untilCmdSent(t, r, `goal "make \"it\" pass"`)
	if c := r.d.next(); c.Payload.(*proto.User).Content != `make "it" pass` {
		t.Fatalf("sent %+v", c.Payload)
	}
	run := func(turn int, running bool, stopped string) *proto.Until {
		return &proto.Until{Conds: []proto.UntilCond{{Kind: "goal", Goal: `make "it" pass`}}, Cap: 20, Turn: turn, Running: running, Stopped: stopped}
	}
	r.d.emit(&proto.ControlEvent{ID: "until", Message: "until: goal"}, run(0, false, ""),
		&proto.UserEcho{ID: "m1", Content: `make "it" pass`}, &proto.Turn{Kind: "user"}, run(1, true, ""), &proto.Done{})
	pending(t, ch, "answered while the run goes on")
	r.d.emit(&proto.Turn{Kind: "continue"}, run(2, true, ""), &proto.Done{})
	pending(t, ch, "answered while the run goes on")
	r.d.emit(run(2, false, "goal met: it passes (turn 2 of 20)"))
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
	var text strings.Builder
	for _, u := range r.ed.take() {
		if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
			text.WriteString(u.AgentMessageChunk.Content.Text.Text)
		}
	}
	if s := text.String(); !strings.Contains(s, "until: turn 2 of 20") || !strings.Contains(s, "until: stopped · goal met") || strings.Contains(s, "until: goal") {
		t.Errorf("text %q", s)
	}
}

// An interrupt ends a held prompt cancelled at once.
func TestUntilCancelled(t *testing.T) {
	r, res := configRig(t)
	sid := res.SessionId
	ch := r.prompt(sid, "work")
	r.d.next()
	running := &proto.Until{Conds: []proto.UntilCond{{Kind: "turns", N: 3}}, Cap: 3, Turn: 1, Running: true}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "work"}, &proto.Turn{Kind: "user"}, running, &proto.Done{Reason: "interrupted"})
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonCancelled {
		t.Fatalf("prompt: %+v", res)
	}
}
