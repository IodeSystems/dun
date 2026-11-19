package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// The effort layers: lowest taken, off read as none, a typo said at start;
// the child's typo kept (a spawn refuses it), main's ignored; the source is
// the flag when the flag gave it.
func TestResolveEfforts(t *testing.T) {
	var w strings.Builder
	el := resolveEfforts(dunconfig.Resolved{Effort: "lowest"}, dunconfig.Resolved{Effort: "off"}, "off", &w)
	if el.main != harness.EffortLowest || el.child != harness.EffortOff || el.childSrc != "--child-effort" || w.Len() != 0 {
		t.Errorf("%+v %q", el, w.String())
	}
	el = resolveEfforts(dunconfig.Resolved{Effort: "hgih"}, dunconfig.Resolved{Effort: "hgih"}, "", &w)
	if el.main != "" || el.child != "hgih" || el.childSrc != "facets.child" ||
		!strings.Contains(w.String(), `facets.main.effort "hgih" is not a level`) || !strings.Contains(w.String(), `facets.child "hgih" is not a level`) {
		t.Errorf("%+v %q", el, w.String())
	}
}

// doctor names --child-effort as the source of the child default's effort.
func TestDoctorRoles_ChildEffortSource(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	prev := modelFlags
	defer func() { modelFlags = prev }()
	modelFlags = dunconfig.Overrides{dunconfig.FacetChild: {Effort: "high"}}
	fc := dunconfig.DunConfig{}
	set := fc.ResolveTiers(dunconfig.TierFlags{}, nil, dunconfig.RepoTiers{})
	sides, _ := fc.SideCallTiers()
	var w strings.Builder
	c := doctorRoles(fc, set, sides, &w).Resolve(harness.ChildRole(harness.TierWork), harness.RoleArg{})
	if c.Effort.Wanted != harness.EffortHigh || c.EffortSource != "--child-effort" {
		t.Errorf("%+v", c)
	}
}

// Changes that arrive together are sent in order: the last settings event
// sent is the table as it is now.
func TestRolesResend_Ordered(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := llm.NewClient("http://order.test/v1", "", "g")
	h, err := harness.Start(context.Background(), harness.Config{Workspace: t.TempDir(), Servers: []mcpserver.Server{},
		Client: c, Endpoint: harness.Endpoint{URL: "http://order.test/v1", Model: "g"}})
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
	var mu sync.Mutex
	var lastSent []byte
	resend := rolesResend(func(p any) {
		if s, ok := p.(*proto.Settings); ok {
			mu.Lock()
			lastSent, _ = json.Marshal(s.Tiers)
			mu.Unlock()
		}
	}, func() any { return nil })
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			caps := c.Capabilities()
			if caps.Facts == nil {
				caps.Facts = map[string]llm.Fact{}
			}
			caps.Facts[llm.CapReasoningThinkingOnly] = llm.Fact{Value: strconv.FormatBool(i%2 == 0), Source: llm.SourceRefused}
			c.SetCapabilities(caps)
			resend()
		}()
	}
	wg.Wait()
	resend()
	now, _ := json.Marshal(settingsEvent().Tiers)
	if string(lastSent) != string(now) {
		t.Errorf("the last table sent is not the table now:\nsent %s\nnow  %s", lastSent, now)
	}
}

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
