package acp

import (
	"context"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

func usageEvent(active int, cost float64, cur string) *proto.Usage {
	u := &proto.Usage{Active: active, Spend: &proto.Spend{Currency: cur, Session: &cost},
		TurnStats: &proto.TurnStats{Rounds: 2, Prompt: 1500, Cached: 1000, CacheWrite: 50, Completion: 200}}
	u.Window.Window = 32000
	return u
}

func usageUpdates(us []sdk.SessionUpdate) (out []*sdk.SessionUsageUpdate) {
	for _, u := range us {
		if u.UsageUpdate != nil {
			out = append(out, u.UsageUpdate)
		}
	}
	return
}

// A turn's usage event becomes a usage_update (window, context, cumulative
// dollar cost) and the prompt response's usage (the turn's totals); a round
// finishing mid-turn moves the context.
func TestUsage(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	<-r.d.creates
	r.ed.take()
	done := make(chan sdk.PromptResponse, 1)
	go func() {
		res, err := r.cc.Prompt(context.Background(), sdk.PromptRequest{SessionId: sid, Prompt: []sdk.ContentBlock{sdk.TextBlock("hi")}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "hi"}, &proto.Turn{Kind: "user"},
		&proto.Request{Phase: "done", Prompt: 900, Gen: 100, Window: 32000},
		usageEvent(2100, 0.25, "USD"), &proto.Done{})
	res := <-done
	if res.StopReason != sdk.StopReasonEndTurn || res.Usage == nil {
		t.Fatalf("response %+v", res)
	}
	u := res.Usage
	if u.InputTokens != 1500 || u.OutputTokens != 200 || u.TotalTokens != 1700 ||
		u.CachedReadTokens == nil || *u.CachedReadTokens != 1000 || u.CachedWriteTokens == nil || *u.CachedWriteTokens != 50 {
		t.Errorf("usage %+v", u)
	}
	ups := usageUpdates(r.ed.take())
	if len(ups) != 2 || ups[0].Used != 1000 || ups[0].Size != 32000 || ups[0].Cost != nil {
		t.Fatalf("round update: %+v", ups)
	}
	if ups[1].Used != 2100 || ups[1].Cost == nil || ups[1].Cost.Amount != 0.25 || ups[1].Cost.Currency != "USD" {
		t.Errorf("turn update: %+v", ups[1])
	}
}

// A cost not in dollars is left out; so is the turn's usage when dun gave
// no turn stats.
func TestUsageNoCost(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	<-r.d.creates
	r.ed.take()
	done := make(chan sdk.PromptResponse, 1)
	go func() {
		res, _ := r.cc.Prompt(context.Background(), sdk.PromptRequest{SessionId: sid, Prompt: []sdk.ContentBlock{sdk.TextBlock("hi")}})
		done <- res
	}()
	r.d.next()
	u := usageEvent(500, 3, "credits")
	u.TurnStats = nil
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "hi"}, &proto.Turn{Kind: "user"}, u, &proto.Done{})
	if res := <-done; res.Usage != nil {
		t.Errorf("usage %+v", res.Usage)
	}
	if ups := usageUpdates(r.ed.take()); len(ups) != 1 || ups[0].Cost != nil || ups[0].Used != 500 {
		t.Errorf("updates %+v", ups)
	}
}

// session/load sends one usage_update with the session's last usage.
func TestUsageLoad(t *testing.T) {
	r := newRig(t)
	r.d.state = []any{usageEvent(4000, 1.5, "USD")}
	if _, err := r.cc.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "s1", Cwd: r.cwd, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	ups := usageUpdates(r.ed.take())
	if len(ups) != 1 || ups[0].Used != 4000 || ups[0].Size != 32000 || ups[0].Cost == nil || ups[0].Cost.Amount != 1.5 {
		t.Fatalf("updates %+v", ups)
	}
}

func TestAddUsageSumsHeldTurns(t *testing.T) {
	two := int64(2)
	a := &sdk.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11, CachedReadTokens: &two}
	b := &sdk.Usage{InputTokens: 20, OutputTokens: 2, TotalTokens: 22}
	got := addUsage(a, b)
	if got.InputTokens != 30 || got.OutputTokens != 3 || got.TotalTokens != 33 || got.CachedReadTokens == nil || *got.CachedReadTokens != 2 || got.CachedWriteTokens != nil {
		t.Fatalf("addUsage = %+v", got)
	}
	if addUsage(nil, b) != b || addUsage(a, nil) != a {
		t.Fatal("nil side should return the other")
	}
}
