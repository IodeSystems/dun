package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

var spendNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func fullSpend() *proto.Spend {
	return &proto.Spend{
		Currency: "USD", Session: ptr(1234.5), Calls: 14, CostedCalls: 12,
		LastTurn: ptr(0.0042), LastTurnCalls: 3, LastTurnCosted: 3,
		Key: &proto.KeyBudget{Spend: 45.5, Limit: ptr(50.0), Remaining: ptr(4.5),
			ResetAt: "2026-11-01T00:00:00Z", Interval: "30d", Currency: "USD", Source: "litellm",
			AsOf: "2026-10-22T11:58:00Z"},
		Limits: []proto.RateLimit{
			{Name: "requests", Limit: ptr(int64(60)), Remaining: ptr(int64(59)), ResetAt: "2026-10-22T12:00:01Z"},
			{Name: "tokens", Limit: ptr(int64(150000)), Remaining: ptr(int64(149984)), ResetAt: "2026-10-22T11:59:00Z"},
		},
	}
}

// valueEnd is where a /context row's value column ends: indent, label, value.
const valueEnd = 2 + ctxLabelW + 9

func TestCostBlockAlignedAndGrouped(t *testing.T) {
	s := contextStats{spend: fullSpend()}
	out := stripANSI(costBlock(&s, spendNow))
	for _, want := range []string{
		"cost & limits",
		"session spend", "1,234.50 USD · 12 of 14 calls priced",
		"last turn", "0.00 USD (0.0042)",
		"key spend", "45.50 USD · as of 2m ago",
		"key limit", "50.00 USD",
		"key remaining", "4.50 USD (9%)",
		"key resets", "in 9d 12h · 2026-11-01 00:00 UTC · every 30d",
		"requests left", "59 of 60 (98%) · resets in 1s",
		"tokens left", "149,984 of 150,000 (100%) · reset 1m ago (stale)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Every row's value ends in one column, so the amounts line up on their
	// last digit.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.Contains(line, "cost & limits") || strings.TrimSpace(line) == "" {
			continue
		}
		r := []rune(line)
		if len(r) < valueEnd || r[valueEnd-1] == ' ' || (len(r) > valueEnd && r[valueEnd] != ' ') {
			t.Errorf("value not in its column: %q", line)
		}
	}
}

// What the endpoint did not report reads "not reported", never 0.
func TestCostBlockNotReported(t *testing.T) {
	for name, sp := range map[string]*proto.Spend{
		"nothing yet":   nil,
		"unpriced":      {Calls: 4},
		"unsupported":   {Calls: 4, KeyUnsupported: true},
		"read failed":   {KeyError: "status 500"},
		"key, no limit": {Key: &proto.KeyBudget{Spend: 2, Currency: "USD"}},
	} {
		t.Run(name, func(t *testing.T) {
			out := stripANSI(costBlock(&contextStats{spend: sp}, spendNow))
			if !strings.Contains(out, "not reported") {
				t.Errorf("no 'not reported' in:\n%s", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "session spend") || strings.Contains(line, "last turn") ||
					strings.Contains(line, "rate limits") || strings.Contains(line, "key limit") {
					if strings.Contains(line, "0.00") || !strings.Contains(line, "not reported") {
						t.Errorf("unreported value rendered as a number: %q", line)
					}
				}
			}
		})
	}
	out := stripANSI(costBlock(&contextStats{spend: &proto.Spend{KeyUnsupported: true}}, spendNow))
	if !strings.Contains(out, "the endpoint has none") {
		t.Errorf("unsupported not explained:\n%s", out)
	}
}

func TestSpendWarningThresholds(t *testing.T) {
	key := func(remaining, limit float64) *proto.Spend {
		return &proto.Spend{Key: &proto.KeyBudget{Limit: ptr(limit), Remaining: ptr(remaining)}}
	}
	rate := func(remaining, limit int64, reset string) *proto.Spend {
		return &proto.Spend{Limits: []proto.RateLimit{{Name: "requests", Limit: ptr(limit), Remaining: ptr(remaining), ResetAt: reset}}}
	}
	for _, c := range []struct {
		name string
		sp   *proto.Spend
		want string
		loud bool
	}{
		{"none", nil, "", false},
		{"key plenty", key(20, 100), "", false},
		{"key at 10%", key(10, 100), "", false},
		{"key under 10%", key(8, 100), "key budget 8% left", false},
		{"key under 2%", key(1, 100), "key budget 1% left", true},
		{"key empty", key(0, 100), "key budget 0% left", true},
		{"no limit", &proto.Spend{Key: &proto.KeyBudget{Remaining: ptr(1.0)}}, "", false},
		{"rate under 10%", rate(5, 100, "2026-10-22T12:00:30Z"), "requests 5% left", false},
		{"rate empty", rate(0, 100, ""), "requests 0% left", true},
		{"rate reset passed", rate(0, 100, "2026-10-22T11:59:00Z"), "", false},
	} {
		got, loud := spendWarning(c.sp, spendNow)
		if got != c.want || loud != c.loud {
			t.Errorf("%s: got %q loud=%v, want %q loud=%v", c.name, got, loud, c.want, c.loud)
		}
	}
	// The scarcest wins.
	sp := key(8, 100)
	sp.Limits = rate(3, 100, "").Limits
	if got, _ := spendWarning(sp, spendNow); got != "requests 3% left" {
		t.Errorf("scarcest = %q", got)
	}
}

// The snapshot → wire mapping keeps "not reported" absent.
func TestSpendProto(t *testing.T) {
	m := dun.NewSpendMeter()
	if p := spendProto(m.Snapshot(), spendNow); p.Session != nil || p.LastTurn != nil || p.Key != nil || p.Limits != nil {
		t.Errorf("empty meter = %+v", p)
	}
	m.Note(llm.Usage{
		Cost: llm.Cost{Amount: 0.5, Currency: "USD", Reported: true},
		Limits: llm.RateLimits{Reported: true,
			Requests: llm.RateWindow{Remaining: 0, HasRemaining: true, Reported: true}},
		KeyBudget: llm.KeyBudget{Spend: 3, Currency: "USD", Source: llm.BudgetSourceLiteLLMHeaders, Reported: true},
	})
	m.EndTurn()
	p := spendProto(m.Snapshot(), spendNow)
	if p.Session == nil || *p.Session != 0.5 || p.LastTurn == nil || *p.LastTurn != 0.5 {
		t.Errorf("costs = %+v", p)
	}
	if p.Key == nil || p.Key.Limit != nil || p.Key.Remaining != nil || p.Key.ResetAt != "" {
		t.Errorf("key = %+v", p.Key)
	}
	if len(p.Limits) != 1 || p.Limits[0].Limit != nil || p.Limits[0].Remaining == nil || *p.Limits[0].Remaining != 0 {
		t.Errorf("limits = %+v", p.Limits)
	}
}

// The wire round trip: a usage event's spend and a spend event both reach
// /context, and absent fields stay absent on the wire.
func TestSpendEventRoundTrip(t *testing.T) {
	sp := fullSpend()
	for _, payload := range []any{&proto.Usage{Spend: sp}, sp} {
		b, err := json.Marshal(proto.NewEvent(payload))
		if err != nil {
			t.Fatal(err)
		}
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
		var s contextStats
		s.readSpend(ev)
		if s.spend == nil || *s.spend.Session != 1234.5 || s.spend.Key.Interval != "30d" || len(s.spend.Limits) != 2 {
			t.Errorf("%s: read %+v", ev["type"], s.spend)
		}
	}
	b, _ := json.Marshal(&proto.Spend{Calls: 2})
	for _, absent := range []string{"session_cost", "last_turn_cost", "key", "limits"} {
		if strings.Contains(string(b), `"`+absent+`"`) {
			t.Errorf("unreported %q on the wire: %s", absent, b)
		}
	}
}

// Every client the per-model cache builds reports to the meter.
func TestClientCacheWiresOnUsage(t *testing.T) {
	var got int
	get := clientCache("http://127.0.0.1:1", "k", func(llm.Usage) { got++ })
	r, err := get("child-model")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := r.(*llm.Client)
	if !ok || c.OnUsage == nil {
		t.Fatal("cached client has no OnUsage")
	}
	c.OnUsage(llm.Usage{})
	if got != 1 {
		t.Error("OnUsage is not the meter's")
	}
}

// No cost on any call: the hint names the gateway's reason.
func TestCostBlock_NoCostHint(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		key  *proto.KeyBudget
		want string
	}{
		{&proto.KeyBudget{Source: "litellm", Currency: "USD"}, "include_cost_in_streaming_usage"},
		{&proto.KeyBudget{Source: "litellm-headers", Currency: "USD"}, "include_cost_in_streaming_usage"},
		{&proto.KeyBudget{Source: "openrouter", Currency: "credits"}, "openrouter.ai"},
		{nil, "sends no cost with its responses"},
	} {
		out := stripANSI(costBlock(&contextStats{spend: &proto.Spend{Calls: 3, Key: c.key}}, now))
		if !strings.Contains(out, c.want) {
			t.Errorf("%+v: no %q in\n%s", c.key, c.want, out)
		}
	}
	// Priced, or nothing sent yet: no hint.
	one := 0.01
	for _, sp := range []*proto.Spend{{Calls: 2, CostedCalls: 2, Session: &one}, {Calls: 0}} {
		if out := stripANSI(costBlock(&contextStats{spend: sp}, now)); strings.Contains(out, "sends no cost") || strings.Contains(out, "include_cost") {
			t.Errorf("hint shown for %+v:\n%s", sp, out)
		}
	}
}
