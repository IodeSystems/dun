package main

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// The settings event carries the role table after the tiers, and is sent
// again when an effort translation (agentkit OnTranslate) changes what a
// role's requests send; a translation that changes nothing sends nothing.
func TestRolesResend_OnTranslate(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := llm.NewClient("http://only.test/v1", "", "g")
	harness.TrackCaps(c, "")
	h, err := harness.Start(context.Background(), harness.Config{Workspace: t.TempDir(), Servers: []mcpserver.Server{},
		Client: c, Endpoint: harness.Endpoint{URL: "http://only.test/v1", Model: "g"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	prev := enginecmd.EngineHarness.Load()
	enginecmd.EngineHarness.Store(h)
	defer func() {
		if prev != nil {
			enginecmd.EngineHarness.Store(prev)
		}
	}()
	sent := make(chan *proto.Settings, 4)
	resend := rolesResend(func(p any) {
		if s, ok := p.(*proto.Settings); ok {
			sent <- s
		}
	}, func() any { return nil })
	harness.OnCapsChanged(resend)
	defer harness.OnCapsChanged(nil)

	fold := func(s *proto.Settings) proto.TierInfo {
		for _, r := range s.Tiers {
			if r.Role == "fold" {
				return r
			}
		}
		t.Fatalf("no fold row in %+v", s.Tiers)
		return proto.TierInfo{}
	}
	resend()
	first := <-sent
	if r := fold(first); r.Wanted != harness.EffortLowest || r.Effective != harness.EffortOff {
		t.Errorf("fold before: %+v", r)
	}
	// Nothing changed: no event.
	c.OnTranslate(llm.Translation{Key: "effort", Sent: "none", Wanted: "none"})
	select {
	case s := <-sent:
		t.Fatalf("sent with no change: %+v", s)
	case <-time.After(200 * time.Millisecond):
	}
	// The record learns the model cannot stop thinking: fold now sends minimal.
	caps := c.Capabilities()
	caps.Facts[llm.CapReasoningThinkingOnly] = llm.Fact{Value: "true", Source: llm.SourceRefused}
	c.SetCapabilities(caps)
	c.OnTranslate(llm.Translation{Key: "effort", Sent: "minimal", Wanted: "none", Reason: "minimal (none not supported)"})
	select {
	case s := <-sent:
		if r := fold(s); r.Effective != harness.EffortMinimal || r.Reason == "" {
			t.Errorf("fold after: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not sent again after the translation")
	}
}
