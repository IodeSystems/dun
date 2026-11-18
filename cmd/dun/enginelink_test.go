package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// linkDir is a short directory for a link's socket (sun_path is ~104 bytes).
func linkDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "el")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testLink(t *testing.T) *engineLink {
	t.Helper()
	l, err := listenLink(linkDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

// emitN writes n token events numbered from first.
func emitN(l *engineLink, first, n int) {
	for i := first; i < first+n; i++ {
		ev := proto.NewEvent(&proto.Token{Text: "t" + strconv.Itoa(i)})
		b, _ := json.Marshal(ev)
		_, _ = l.Write(append(b, '\n'))
	}
}

type linkDialer struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
}

func dialLink(t *testing.T, l *engineLink) (*linkDialer, proto.LinkHello) {
	t.Helper()
	c, err := net.Dial("unix", l.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	d := &linkDialer{t: t, c: c, sc: bufio.NewScanner(c)}
	d.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var h proto.LinkHello
	if err := json.Unmarshal([]byte(d.line()), &h); err != nil || h.Type != proto.LinkHelloType {
		t.Fatalf("hello: %v %+v", err, h)
	}
	return d, h
}

func (d *linkDialer) line() string {
	d.t.Helper()
	_ = d.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !d.sc.Scan() {
		d.t.Fatalf("link: no line: %v", d.sc.Err())
	}
	return d.sc.Text()
}

func (d *linkDialer) send(v any) {
	d.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := d.c.Write(append(b, '\n')); err != nil {
		d.t.Fatal(err)
	}
}

// until reads lines up to link_live: the seqs and texts of the events.
func (d *linkDialer) replay() (seqs []uint64, texts []string, other []string) {
	d.t.Helper()
	for {
		ln := d.line()
		if ln == proto.LinkLiveLine {
			return
		}
		if strings.HasPrefix(ln, "{") {
			other = append(other, ln)
			continue
		}
		n, ev, _ := strings.Cut(ln, " ")
		seq, _ := strconv.ParseUint(n, 10, 64)
		var e proto.Event
		_ = json.Unmarshal([]byte(ev), &e)
		if tok, ok := e.Payload.(*proto.Token); ok {
			seqs, texts = append(seqs, seq), append(texts, tok.Text)
		} else {
			other = append(other, n+" "+e.Type)
		}
	}
}

func wantTokens(t *testing.T, got []string, from, to int) {
	t.Helper()
	var want []string
	for i := from; i <= to; i++ {
		want = append(want, "t"+strconv.Itoa(i))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want t%d..t%d", got, from, to)
	}
}

// waitDetached waits until the engine saw the dialer go.
func waitDetached(t *testing.T, l *engineLink) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.connected() || l.isClaimed() {
		if time.Now().After(deadline) {
			t.Fatal("link still attached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *engineLink) isClaimed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.claimed
}

// A resume after seq N sends N+1 on, each once: what the first dund acked
// is not sent again, what it did not take and what came in the gap is.
func TestLinkResumeAfterSeqNoLossNoDup(t *testing.T) {
	l := testLink(t)
	emitN(l, 1, 10)
	d, h := dialLink(t, l)
	if h.Seq != 10 || h.Acked != 0 || h.Pid != os.Getpid() || h.Link != proto.LinkVersion || h.Proto != proto.Version {
		t.Fatalf("hello = %+v", h)
	}
	d.send(proto.LinkResume{Type: proto.LinkResumeType, After: 0})
	seqs, texts, _ := d.replay()
	wantTokens(t, texts, 1, 10)
	if seqs[0] != 1 || seqs[9] != 10 {
		t.Fatalf("seqs = %v", seqs)
	}
	emitN(l, 11, 2) // live
	for i, want := range []string{"11 ", "12 "} {
		if ln := d.line(); !strings.HasPrefix(ln, want) {
			t.Fatalf("live %d = %q", i, ln)
		}
	}
	d.send(proto.LinkAck{Type: proto.LinkAckType, Seq: 7})
	d.c.Close()
	waitDetached(t, l)
	emitN(l, 13, 3) // the gap

	d2, h2 := dialLink(t, l)
	if h2.Acked != 7 || h2.Seq != 15 || h2.First != 8 {
		t.Fatalf("second hello = %+v", h2)
	}
	d2.send(proto.LinkResume{Type: proto.LinkResumeType, After: h2.Acked})
	seqs, texts, other := d2.replay()
	wantTokens(t, texts, 8, 15)
	if len(other) != 0 || seqs[0] != 8 {
		t.Fatalf("seqs %v, other %v", seqs, other)
	}
}

// With no dund attached the ring keeps the newest events within its cap;
// the next resume is told what is gone (link_gap), then gets what is kept.
func TestLinkRingOverflowSaysGap(t *testing.T) {
	old := linkRingCap
	linkRingCap = 1000
	t.Cleanup(func() { linkRingCap = old })
	l := testLink(t)
	emitN(l, 1, 100) // ~100 bytes each, no dund
	d, h := dialLink(t, l)
	if h.First <= 1 || h.Seq != 100 {
		t.Fatalf("hello = %+v", h)
	}
	d.send(proto.LinkResume{Type: proto.LinkResumeType, After: 0})
	_, texts, other := d.replay()
	if len(other) != 1 {
		t.Fatalf("want one link_gap, got %v", other)
	}
	var g proto.LinkGap
	if err := json.Unmarshal([]byte(other[0]), &g); err != nil || g.Type != proto.LinkGapType || g.After != 0 || g.First != h.First {
		t.Fatalf("gap = %+v (%v)", g, err)
	}
	wantTokens(t, texts, int(h.First), 100)
}

// An adoption asks for the state too: the latest event of each state type
// at or before after, unnumbered, before the replay.
func TestLinkResumeSendsState(t *testing.T) {
	l := testLink(t)
	for _, p := range []any{&proto.TurnState{Phase: proto.PhaseIdle}, &proto.Token{Text: "t1"},
		&proto.TurnState{Phase: proto.PhaseWorking}, &proto.Token{Text: "t2"}, &proto.Jobs{}} {
		b, _ := json.Marshal(proto.NewEvent(p))
		_, _ = l.Write(append(b, '\n'))
	}
	l.ack(4)
	d, _ := dialLink(t, l)
	d.send(proto.LinkResume{Type: proto.LinkResumeType, After: 4, State: true})
	_, texts, other := d.replay()
	// turn_state (seq 3) is state at or before 4; jobs (5) is replayed.
	if strings.Join(other, ",") != "0 turn_state,5 jobs" || len(texts) != 0 {
		t.Fatalf("other = %v, texts = %v", other, texts)
	}
}

// One dund at a time: a second dialer is refused.
func TestLinkRefusesASecondDialer(t *testing.T) {
	l := testLink(t)
	d, _ := dialLink(t, l)
	d.send(proto.LinkResume{Type: proto.LinkResumeType})
	d.replay()
	c, err := net.Dial("unix", l.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !sc.Scan() || sc.Text() != proto.LinkBusyLine {
		t.Fatalf("second dialer got %q", sc.Text())
	}
}

// Commands reach the engine's input; acks do not.
func TestLinkCommandsReachTheInput(t *testing.T) {
	l := testLink(t)
	d, _ := dialLink(t, l)
	d.send(proto.LinkResume{Type: proto.LinkResumeType})
	d.replay()
	d.send(proto.LinkAck{Type: proto.LinkAckType, Seq: 0})
	if _, err := d.c.Write([]byte(`{"type":"user","content":"hi"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(l.in)
	if !sc.Scan() || sc.Text() != `{"type":"user","content":"hi"}` {
		t.Fatalf("input = %q", sc.Text())
	}
}

// No dund for linkOrphanWait: the engine is told to stop.
func TestLinkOrphanStops(t *testing.T) {
	old := linkOrphanWait
	linkOrphanWait = 50 * time.Millisecond
	t.Cleanup(func() { linkOrphanWait = old })
	l := testLink(t)
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(l.in)
		sc.Scan()
		got <- sc.Text()
	}()
	select {
	case ln := <-got:
		if ln != `{"type":"stop"}` {
			t.Fatalf("input = %q", ln)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stop")
	}
}

// Close sends what is left and removes the socket and the entry; the entry
// names the session once it is set.
func TestLinkCloseFlushesAndCleansUp(t *testing.T) {
	l := testLink(t)
	l.setSession("s1")
	b, err := os.ReadFile(l.infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var info proto.EngineInfo
	if json.Unmarshal(b, &info) != nil || info.Session != "s1" || info.Pid != os.Getpid() || info.Sock != l.sock {
		t.Fatalf("info = %s", b)
	}
	d, _ := dialLink(t, l)
	d.send(proto.LinkResume{Type: proto.LinkResumeType})
	d.replay()
	emitN(l, 1, 3)
	// A partial line at exit is written too.
	_, _ = l.Write([]byte(`{"v":1,"type":"done"}`))
	l.Close()
	var lines []string
	_ = d.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for d.sc.Scan() {
		lines = append(lines, d.sc.Text())
	}
	if len(lines) != 4 || !strings.HasPrefix(lines[3], "4 ") {
		t.Fatalf("lines = %q", lines)
	}
	for _, p := range []string{l.sock, l.infoPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	if _, err := l.in.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Errorf("input read after close: %v", err)
	}
}

// A dialer attached and slow: the writer waits instead of dropping.
func TestLinkBackpressureWhileAttached(t *testing.T) {
	old := linkRingCap
	linkRingCap = 1000
	t.Cleanup(func() { linkRingCap = old })
	l := testLink(t)
	d, _ := dialLink(t, l)
	d.send(proto.LinkResume{Type: proto.LinkResumeType})
	d.replay()
	done := make(chan struct{})
	go func() {
		emitN(l, 1, 50)
		close(done)
	}()
	var texts []string
	for len(texts) < 50 {
		ln := d.line()
		n, ev, _ := strings.Cut(ln, " ")
		var e proto.Event
		_ = json.Unmarshal([]byte(ev), &e)
		texts = append(texts, e.Payload.(*proto.Token).Text)
		if len(texts)%5 == 0 {
			seq, _ := strconv.ParseUint(n, 10, 64)
			d.send(proto.LinkAck{Type: proto.LinkAckType, Seq: seq})
		}
	}
	<-done
	wantTokens(t, texts, 1, 50)
}

func TestDropListen(t *testing.T) {
	got := dropListen([]string{"--engine", "--listen", "/x", "--model", "m", "--listen=/y", "-listen", "/z"})
	if fmt.Sprint(got) != "[--engine --model m]" {
		t.Fatalf("got %v", got)
	}
}
