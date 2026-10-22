package proto

import (
	"errors"
	"sync"
	"testing"
)

func appendTokens(l *Log, n int) {
	for i := 0; i < n; i++ {
		l.Append(NewEvent(&Token{Text: "t"}))
	}
}

func seqs(evs []Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func TestLogNumbersAndStamps(t *testing.T) {
	l := NewLog("s1", 10)
	a := l.Append(NewEvent(&Token{Text: "a"}))
	b := l.Append(NewEvent(&Done{}))
	if a.Seq != 1 || b.Seq != 2 || a.Session != "s1" || a.TS.IsZero() || a.V != Version || l.Last() != 2 {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestLogReplayAfter(t *testing.T) {
	l := NewLog("s", 10)
	appendTokens(l, 5)
	for after, want := range map[uint64]int{0: 5, 3: 2, 5: 0} {
		replay, sub, err := l.Subscribe(after, 4)
		if err != nil {
			t.Fatal(err)
		}
		sub.Close()
		if len(replay) != want || (want > 0 && replay[0].Seq != after+1) {
			t.Errorf("after %d: %v", after, seqs(replay))
		}
	}
	if _, _, err := l.Subscribe(6, 4); !errors.Is(err, ErrFuture) {
		t.Errorf("after 6: %v", err)
	}
}

// Past the ring, the oldest are gone: asking from before them is a gap, the
// first kept one still replays.
func TestLogGapAfterWrap(t *testing.T) {
	l := NewLog("s", 4)
	appendTokens(l, 10) // keeps 7..10
	if _, _, err := l.Subscribe(5, 4); !errors.Is(err, ErrGap) {
		t.Fatalf("after 5: %v", err)
	}
	if _, _, err := l.Subscribe(0, 4); !errors.Is(err, ErrGap) {
		t.Fatalf("after 0: %v", err)
	}
	replay, sub, err := l.Subscribe(6, 4)
	if err != nil {
		t.Fatal(err)
	}
	sub.Close()
	if got := seqs(replay); len(got) != 4 || got[0] != 7 || got[3] != 10 {
		t.Fatalf("after 6: %v", got)
	}
}

// Replay then live, while another goroutine appends: every seq exactly once,
// in order.
func TestLogNoGapNoRepeatUnderAppend(t *testing.T) {
	l := NewLog("s", 10000)
	appendTokens(l, 100)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		appendTokens(l, 2000)
	}()
	replay, sub, err := l.Subscribe(50, 4096)
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	sub.Close()
	got := seqs(replay)
	for ev := range sub.C() {
		got = append(got, ev.Seq)
	}
	if len(got) != 2050 {
		t.Fatalf("got %d events", len(got))
	}
	for i, s := range got {
		if s != uint64(51+i) {
			t.Fatalf("index %d: seq %d", i, s)
		}
	}
}

// A subscriber that stops reading is dropped, and the others keep getting
// events.
func TestLogDropsSlowSubscriber(t *testing.T) {
	l := NewLog("s", 100)
	_, slow, _ := l.Subscribe(0, 2)
	_, fast, _ := l.Subscribe(0, 100)
	appendTokens(l, 5)
	n := 0
	for range slow.C() {
		n++
	}
	if n != 2 || !errors.Is(slow.Err(), ErrSlow) {
		t.Fatalf("slow: read %d, err %v", n, slow.Err())
	}
	if len(fast.C()) != 5 || fast.Err() != nil {
		t.Fatalf("fast: %d queued, err %v", len(fast.C()), fast.Err())
	}
}

func TestLogStateIsLatestPerType(t *testing.T) {
	l := NewLog("s", 3)
	l.Append(NewEvent(&Session{ID: "x"}))
	l.Append(NewEvent(&Jobs{}))
	l.Append(NewEvent(&Token{Text: "not state"}))
	l.Append(NewEvent(&Jobs{Jobs: []JobInfo{{ID: 1}}}))
	appendTokens(l, 5) // the ring no longer holds any of them
	st := l.State()
	if len(st) != 2 || st[0].Type != "session" || st[1].Type != "jobs" || st[1].Seq != 4 {
		t.Fatalf("state: %+v", st)
	}
}

func TestLogClose(t *testing.T) {
	l := NewLog("s", 4)
	_, sub, _ := l.Subscribe(0, 4)
	l.Close()
	if _, ok := <-sub.C(); ok || !errors.Is(sub.Err(), ErrClosed) {
		t.Fatalf("err %v", sub.Err())
	}
	sub.Close() // after the log closed: no panic
	if _, _, err := l.Subscribe(0, 4); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after close: %v", err)
	}
	l.Append(NewEvent(&Done{})) // no subscribers left; must not panic
}
