package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
	"github.com/iodesystems/dun/proto"
)

// engine is one engine process's tally state on a session.
type engine struct {
	t     *sessionTally
	u     *usageTally
	meter *spendmeter.Meter
	note  func(llm.Usage)
	seed  string
}

func startEngine(dir, model string) *engine {
	ledger := spendmeter.NewLedger(filepath.Join(dir, "spend"))
	m := spendmeter.NewMeter()
	m.SetLedger(ledger, "fp")
	m.SetSession("s1")
	e := &engine{t: &sessionTally{reqs: &proto.RequestFold{}, spend: m}, u: &usageTally{}, meter: m,
		note: m.NoteAs(spendmeter.Source{Provider: "p", Key: "fp", Model: model})}
	e.seed = seedTally(e.t, e.u, filepath.Join(dir, "s1.stats.json"), ledger.SessionEntries("s1"), nil)
	return e
}

// round is one model round: its request events, its response, agentkit's
// tally after it (raw: this process's), and the stats file saved.
func (e *engine) round(raw agent.TokenUsage, prompt, cached, gen int, pp, tg float64, cost float64) *proto.Request {
	e.t.reqs.Note(&proto.Request{Phase: "start"})
	done := &proto.Request{Phase: "done", Prompt: prompt, Cached: cached, CacheReported: true, PPS: pp, Gen: gen, TGS: tg}
	e.t.reqs.Note(done)
	e.note(llm.Usage{PromptTokens: prompt, CompletionTokens: gen,
		PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: cached},
		Cost:                llm.Cost{Amount: cost, Currency: "USD", Reported: true}})
	e.t.save(e.u.total(raw))
	return done
}

// endTurn is the turn's usage event: the turn closed, the file saved.
func (e *engine) endTurn(raw agent.TokenUsage) agent.TokenUsage {
	u := e.u.at(raw)
	e.meter.EndTurn()
	e.t.save(u)
	return u
}

// Restart: the next engine's first usage (a turn cut before its first
// round) and first request event carry the totals the last engine ended on;
// a model switch's engine continues them, one total.
func TestTally_ContinuesAcrossRestartAndModelSwitch(t *testing.T) {
	dir := t.TempDir()
	one := startEngine(dir, "m1")
	if one.seed != seedNone {
		t.Fatalf("a new session seeded from %q", one.seed)
	}
	one.round(agent.TokenUsage{Total: 110, Cached: 0, Processed: 100, Generated: 10, Turns: 1}, 100, 0, 10, 500, 40, 0.1)
	one.endTurn(agent.TokenUsage{Total: 110, Processed: 100, Generated: 10, Turns: 1, Active: 110})
	one.round(agent.TokenUsage{Total: 250, Cached: 100, Processed: 120, Generated: 30, Turns: 2}, 220, 100, 30, 300, 20, 0.2)
	before := one.endTurn(agent.TokenUsage{Total: 250, Cached: 100, Processed: 120, Generated: 30, Turns: 2, Active: 250})
	reqBefore := one.t.reqs.State()
	spendBefore := one.meter.Snapshot()

	two := startEngine(dir, "m1") // restart
	if two.seed != seedStats {
		t.Fatalf("seeded from %q", two.seed)
	}
	first := two.u.at(agent.TokenUsage{Active: 250}) // cut before its first round
	if first != before {
		t.Errorf("first usage after restart = %+v, want %+v", first, before)
	}
	start := &proto.Request{Phase: "start"}
	two.t.reqs.Note(start)
	if want := reqBefore.Session; !reflect.DeepEqual(*start.Session, want) {
		t.Errorf("first session_stats = %+v, want %+v", *start.Session, want)
	}
	if s := two.meter.Snapshot(); s.Session != spendBefore.Session || s.LastTurn != spendBefore.LastTurn || s.Calls != spendBefore.Calls {
		t.Errorf("spend after restart = %v/%v/%d, want %v/%v/%d", s.Session, s.LastTurn, s.Calls,
			spendBefore.Session, spendBefore.LastTurn, spendBefore.Calls)
	}

	three := startEngine(dir, "m2") // a model switch: another engine, the same session
	three.round(agent.TokenUsage{Total: 60, Processed: 50, Generated: 10, Turns: 1}, 50, 0, 10, 100, 10, 0.05)
	got := three.endTurn(agent.TokenUsage{Total: 60, Processed: 50, Generated: 10, Turns: 1, Active: 300})
	want := agent.TokenUsage{Total: 310, Cached: 100, Processed: 170, Generated: 40, Turns: 3, Active: 300}
	if got != want {
		t.Errorf("after the switch = %+v, want %+v", got, want)
	}
	// tg's average is weighted by tokens over all three rounds:
	// (40·10 + 20·30 + 10·10) / 50.
	if s := three.t.reqs.State().Session; s.TGAvg != 22 {
		t.Errorf("tg_avg = %v, want 22", s.TGAvg)
	}
	if s := three.meter.Snapshot(); s.Session < 0.349 || s.Session > 0.351 || s.LastTurn < 0.0499 || s.LastTurn > 0.0501 {
		t.Errorf("spend after the switch: session %v last %v", s.Session, s.LastTurn)
	}
}

// Fallback: an older engine's session (no stats file) seeds its tokens from
// the history's per-entry usage, its averages from the rounds with server
// timings; one with neither starts from 0.
func TestTally_Fallbacks(t *testing.T) {
	dir := t.TempDir()
	tl := &sessionTally{reqs: &proto.RequestFold{}, spend: spendmeter.NewMeter()}
	u := &usageTally{}
	if got := seedTally(tl, u, filepath.Join(dir, "none.stats.json"), nil, nil); got != seedNone {
		t.Errorf("no file, no rounds: %q", got)
	}
	if got := u.at(agent.TokenUsage{}); got != (agent.TokenUsage{}) {
		t.Errorf("from 0: %+v", got)
	}

	tl = &sessionTally{reqs: &proto.RequestFold{}, spend: spendmeter.NewMeter()}
	u = &usageTally{}
	rounds := []agent.EntryUsage{
		{Processed: 100, Generated: 10, PromptN: 100, PromptMS: 200, PredictedN: 10, PredictedMS: 500},
		{Cached: 100, Processed: 20, Generated: 30}, // no timings: no rate
	}
	if got := seedTally(tl, u, filepath.Join(dir, "old.stats.json"), nil, rounds); got != seedHistory {
		t.Errorf("old session: %q", got)
	}
	want := agent.TokenUsage{Total: 260, Cached: 100, Processed: 120, Generated: 40, Turns: 2}
	if got := u.at(agent.TokenUsage{}); got != want {
		t.Errorf("from history = %+v, want %+v", got, want)
	}
	s := tl.reqs.State().Session
	if s.PPAvg != 500 || s.TGAvg != 20 || s.CachePct == nil || *s.CachePct != 83 {
		t.Errorf("history fold = %+v", s)
	}
	if c := tl.spend.Snapshot(); c.Calls != 2 || c.Prompt != 220 || c.Completion != 40 {
		t.Errorf("meter = %+v", c)
	}
}
