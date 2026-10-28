package dun

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// fakeClock is a time the test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type reqLog struct {
	mu  sync.Mutex
	evs []RequestStat
}

func (l *reqLog) on(r RequestStat) { l.mu.Lock(); l.evs = append(l.evs, r); l.mu.Unlock() }
func (l *reqLog) all() []RequestStat {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RequestStat(nil), l.evs...)
}

func testMeter(clk *fakeClock, log *reqLog) *reqMeter {
	return &reqMeter{on: log.on, now: clk.now,
		cpt: func() float64 { return 4 }, window: func() int { return 131072 }, est: func() int { return 1000 }}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// A scripted request through the turn's tee, its timing controlled: ttft from
// the send to the first delta, pp from the uncached prompt over ttft and tg
// from the completion over first-to-last when the server sends no timings,
// and ctx as the prompt plus the reply.
func TestReqMeter_ScriptedRequest(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	log := &reqLog{}
	src := make(chan llm.StreamChunk)
	tee := &reasoningTee{LLMRunner: chanRunner(src), req: testMeter(clk, log)}
	out, err := tee.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// send passes one chunk through the tee: it is measured before it is
	// forwarded, so the clock can move after the forward.
	send := func(c llm.StreamChunk) { src <- c; <-out }
	clk.add(800 * time.Millisecond)
	send(llm.StreamChunk{Reasoning: strings.Repeat("r", 40)}) // first token: ttft 0.8s
	clk.add(100 * time.Millisecond)
	send(llm.StreamChunk{Content: strings.Repeat("c", 40)}) // throttled
	clk.add(900 * time.Millisecond)
	send(llm.StreamChunk{Content: strings.Repeat("c", 40)}) // live: 30 tokens in 1s
	send(llm.StreamChunk{Done: true, Usage: &llm.Usage{PromptTokens: 5000, CompletionTokens: 31,
		PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 4200}}})
	close(src)
	for range out {
	}

	evs := log.all()
	var phases []string
	for _, e := range evs {
		phases = append(phases, e.Phase)
	}
	if got := strings.Join(phases, ","); got != "start,live,live,done" {
		t.Fatalf("phases %s (the second delta must be throttled)", got)
	}
	if s := evs[0]; s.Ctx != 1000 || s.Window != 131072 {
		t.Errorf("start %+v: ctx should be the build's estimate", s)
	}
	if l := evs[1]; l.TTFTMS != 800 || l.Gen != 10 || l.Ctx != 1010 {
		t.Errorf("first token %+v", l)
	}
	if l := evs[2]; l.Gen != 30 || !near(l.TGS, 30) || l.Ctx != 1030 || l.Exact {
		t.Errorf("live %+v: 120 chars at 4/token over 1s", l)
	}
	d := evs[3]
	if d.TTFTMS != 800 || d.Prompt != 5000 || d.Cached != 4200 || !d.CacheReported {
		t.Errorf("done %+v", d)
	}
	if !near(d.PPS, 800/0.8) || d.PPSServer { // 800 uncached in 0.8s
		t.Errorf("pp %.1f, want 1000 (client side)", d.PPS)
	}
	if !near(d.TGS, 31) || !d.Exact || d.Gen != 31 { // 31 tokens, first to last 1s
		t.Errorf("tg %+v", d)
	}
	if d.Ctx != 5031 {
		t.Errorf("ctx %d, want the reported prompt plus the reply", d.Ctx)
	}
}

// The prefix measured at startup is the floor of the start's estimate.
func TestReqMeter_FirstStartFloor(t *testing.T) {
	log := &reqLog{}
	m := testMeter(&fakeClock{t: time.Unix(1000, 0)}, log)
	m.floor = func() int { return 4700 }
	m.begin()
	if s := log.all()[0]; s.Ctx != 4700 {
		t.Errorf("first start ctx %d, want the 4700 prefix", s.Ctx)
	}
}

// llama.cpp's own timings win over the client's clock.
func TestReqMeter_ServerTimings(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	log := &reqLog{}
	m := testMeter(clk, log)
	m.begin()
	clk.add(2 * time.Second)
	m.chunk(llm.StreamChunk{Content: "hi"})
	m.chunk(llm.StreamChunk{Done: true, Usage: &llm.Usage{PromptTokens: 3000, CompletionTokens: 90,
		Timings: &llm.Timings{PromptN: 2400, PromptMS: 1200, PredictedN: 90, PredictedMS: 2000}}})
	m.end()
	d := log.all()[len(log.all())-1]
	if !near(d.PPS, 2000) || !d.PPSServer || !near(d.TGS, 45) || !d.TGSServer {
		t.Errorf("done %+v: want the server's 2000/s and 45/s", d)
	}
	if d.CacheReported {
		t.Error("no cache field was sent: cache must read as not reported")
	}
}

// The live tg throttle: a steady stream of deltas yields about four updates
// a second, not one per delta.
func TestReqMeter_LiveThrottle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	log := &reqLog{}
	m := testMeter(clk, log)
	m.begin()
	for range 200 { // 2s at a delta every 10ms
		clk.add(10 * time.Millisecond)
		m.chunk(llm.StreamChunk{Content: "abcd"})
	}
	live := 0
	for _, e := range log.all() {
		if e.Phase == "live" {
			live++
		}
	}
	if live < 8 || live > 9 { // the first token, then one per 250ms
		t.Errorf("%d live updates over 2s, want 8-9", live)
	}
}

// A recap or compaction moves the context figure at once; before any request
// there is nothing to move.
func TestReqMeter_AdjustCtx(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	log := &reqLog{}
	m := testMeter(clk, log)
	m.adjustCtx(-500)
	if len(log.all()) != 0 {
		t.Fatal("a ctx event before any request")
	}
	m.begin()
	m.chunk(llm.StreamChunk{Content: "abcd"})
	m.end()
	m.adjustCtx(-400)
	e := log.all()[len(log.all())-1]
	if e.Phase != "ctx" || e.Ctx != 1000+1-400 {
		t.Errorf("after the fold %+v", e)
	}
}

// Through a harness: the turn's rounds report, a side call does not.
func TestHarness_RequestStatsTurnOnly(t *testing.T) {
	dir := t.TempDir()
	log := &reqLog{}
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		Servers:     []Server{},
		Client:      &thinkRunner{chunks: thinkingRound()},
		SessionFile: filepath.Join(dir, "s.jsonl"),
		OnRequest:   log.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Ask(context.Background(), "say hi"); err != nil {
		t.Fatal(err)
	}
	evs := log.all()
	if len(evs) < 2 || evs[0].Phase != "start" || evs[len(evs)-1].Phase != "done" {
		t.Fatalf("turn events %+v", evs)
	}
	if d := evs[len(evs)-1]; d.Prompt != 5 || d.Gen != 3 || !d.Exact {
		t.Errorf("done %+v", d)
	}
	n := len(evs)
	if _, err := h.Rephrase(context.Background(), "please do the thing for me now"); err != nil {
		t.Fatal(err)
	}
	if len(log.all()) != n {
		t.Error("a side call was counted as the session's request")
	}
}
