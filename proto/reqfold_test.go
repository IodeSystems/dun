package proto

import "testing"

func fold(f *RequestFold, r Request) *RequestSession {
	f.Note(&r)
	return r.Session
}

// The session's figures: the last value of each, the token-weighted
// averages over finished requests, the cache.
func TestRequestFold(t *testing.T) {
	var f RequestFold
	done := Request{Phase: "done", TTFTMS: 800, Prompt: 5000, Cached: 4800, CacheReported: true, PPS: 1000, Gen: 900, TGS: 30}
	if s := fold(&f, done); s.TGAvg != 30 || s.PPAvg != 1000 || !s.CacheSeen || s.CachePct == nil || *s.CachePct != 96 {
		t.Fatalf("first done: %+v", s)
	}
	// A start clears nothing; a live request moves what it measured.
	if s := fold(&f, Request{Phase: "start", Ctx: 41700}); s.TTFTMS != 800 || s.PPS != 1000 || s.TGS != 30 || *s.CachePct != 96 {
		t.Fatalf("start: %+v", s)
	}
	if s := fold(&f, Request{Phase: "live", TTFTMS: 12300, Gen: 9, TGS: 150}); s.TGS != 150 || s.TGAvg != 30 || s.TTFTMS != 12300 {
		t.Fatalf("live: %+v", s)
	}
	// Weighted by tokens: 30/s over 900 and 90/s over 100 is 36/s; pp
	// 1000/s over 200 processed and 4000/s over 1.
	s := fold(&f, Request{Phase: "done", Prompt: 3, Cached: 2, CacheReported: true, PPS: 4000, Gen: 100, TGS: 90})
	if s.TGAvg != 36 || *s.CachePct != 67 || s.PPAvg != 204000.0/201 {
		t.Fatalf("second done: %+v", s)
	}
	// Each event carries its own copy.
	*s.CachePct = 1
	if *fold(&f, Request{Phase: "start"}).CachePct != 67 {
		t.Fatal("an event shares the fold's cache figure")
	}
}

// An unreported cache is not 0%: no cache figure until a done reports it.
func TestRequestFold_CacheNotReported(t *testing.T) {
	var f RequestFold
	if s := fold(&f, Request{Phase: "done", Prompt: 10, Gen: 5, TGS: 10}); s.CacheSeen || s.CachePct != nil {
		t.Fatalf("%+v", s)
	}
	if s := fold(&f, Request{Phase: "live", Prompt: 10, CacheReported: true}); !s.CacheSeen || s.CachePct != nil {
		t.Fatalf("live reported: %+v", s)
	}
}
