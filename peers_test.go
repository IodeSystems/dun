package dun

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

type fakePeers struct {
	list []PeerSession
	sent []string
	err  error
}

func (f *fakePeers) List(context.Context) ([]PeerSession, error) { return f.list, f.err }

func (f *fakePeers) Send(_ context.Context, to, text string, wake bool) (PeerSent, error) {
	if f.err != nil {
		return PeerSent{}, f.err
	}
	f.sent = append(f.sent, to+"|"+text+"|"+map[bool]string{true: "wake", false: "quiet"}[wake])
	return PeerSent{ID: "20261021-222836", Name: "dun:222836", Started: wake}, nil
}

func peerCall(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

func TestPeerTools_OnlyForARootWithALink(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	start := func(p PeerLink) []string {
		h, err := Start(context.Background(), Config{Workspace: t.TempDir(), Servers: []Server{}, Peers: p})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		return h.ToolNames()
	}
	// Under dund: listed from the first request, alone or not (a tool that
	// joined later changed the prompt's prefix — USER 2026-10-28).
	if names := start(&fakePeers{}); !slices.Contains(names, "dun_list") || !slices.Contains(names, "dun_msg") {
		t.Errorf("under dund: %v", names)
	}
	if names := start(nil); slices.Contains(names, "dun_list") || slices.Contains(names, "dun_msg") {
		t.Errorf("standalone: %v", names)
	}
	// A sub-agent never gets them: the link is the root's.
	parent := testHarness(t)
	parent.cfg.Peers = &fakePeers{}
	if childConfig(parent, 1, "", nil, isolation{}).Peers != nil {
		t.Error("a child inherits the peer link")
	}
}

func TestDunList_Renders(t *testing.T) {
	f := &fakePeers{list: []PeerSession{
		{ID: "20261021-222836", Name: "dun:222836", Workspace: "/x/dun", Branch: "main", State: "idle", You: true},
		{ID: "20261021-230000", Name: "raglit:230000", Workspace: "/x/raglit", State: "busy", Attached: 2, Task: "fix the index"},
	}}
	d := withPeers(nil, f, nil)
	out, _ := d(context.Background(), peerCall("dun_list", `{}`))
	for _, want := range []string{"dun:222836  id 20261021-222836  idle  (you)", "⎇ main", "raglit:230000", "busy", "2 attached", "task: fix the index"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	f.err = errors.New("dund went away")
	if out, _ := d(context.Background(), peerCall("dun_list", `{}`)); out != "ERROR: dund went away" {
		t.Errorf("error: %q", out)
	}
}

func TestDunMsg_WakeDefaultsTrue(t *testing.T) {
	f := &fakePeers{}
	d := withPeers(nil, f, nil)
	out, _ := d(context.Background(), peerCall("dun_msg", `{"to":"dun:222836","text":"hi"}`))
	if !strings.Contains(out, "a turn started there") {
		t.Errorf("result %q", out)
	}
	_, _ = d(context.Background(), peerCall("dun_msg", `{"to":"x","text":"later","wake":false}`))
	if !slices.Equal(f.sent, []string{"dun:222836|hi|wake", "x|later|quiet"}) {
		t.Errorf("sent %q", f.sent)
	}
	f.err = errors.New("rate limit: at most 6 messages a minute")
	if out, _ := d(context.Background(), peerCall("dun_msg", `{"to":"x","text":"y"}`)); !strings.HasPrefix(out, "ERROR: rate limit") {
		t.Errorf("refusal %q", out)
	}
}

// Busy: the message is lifted into the next tool result, labelled as a peer,
// and frees a waiting exec the way a typed message does.
func TestPeerMessage_BusyLiftsAndFreesAWait(t *testing.T) {
	h := testHarness(t)
	wait := h.userWaiting()
	h.PeerMessage("20261021-230000", "raglit:230000", "is the index rebuilt?", true)
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("a peer message must free a wait")
	}
	out := h.liftQueued("tool output")
	for _, want := range []string{"tool output", "[peer] [dun msg from raglit:230000 (session 20261021-230000)] is the index rebuilt?",
		"not from your user", `dun_msg(to:"20261021-230000")`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// Idle with wake: the driver is woken and the turn has the message.
func TestPeerMessage_IdleWakeStartsATurn(t *testing.T) {
	h := testHarness(t)
	h.PeerMessage("a", "dun:1", "hello", true)
	select {
	case <-h.Wake():
	default:
		t.Fatal("no wake")
	}
	if h.Pending() != 1 {
		t.Fatalf("Pending %d", h.Pending())
	}
	if n := h.flushQueued(); n != 1 {
		t.Fatalf("flushed %d", n)
	}
}

// wake:false: nothing wakes, nothing is pending, no wait is freed — it
// rides the next turn that runs anyway.
func TestPeerMessage_NoWakeWaitsForTheNextTurn(t *testing.T) {
	h := testHarness(t)
	wait := h.userWaiting()
	h.PeerMessage("a", "dun:1", "fyi", false)
	select {
	case <-h.Wake():
		t.Fatal("woken")
	case <-wait:
		t.Fatal("wait freed")
	default:
	}
	if h.Pending() != 0 || h.Queued() != 0 {
		t.Fatalf("Pending %d Queued %d", h.Pending(), h.Queued())
	}
	if h.flushQueued() != 0 || h.flushSilent() != 1 {
		t.Fatal("not carried by the next turn")
	}
}

// An open ask_user stays open: a peer message never answers it, and reaches
// the model only in a result after the question's own.
func TestPeerMessage_DoesNotAnswerAnAsk(t *testing.T) {
	h := testHarness(t)
	answers := make(chan string)
	asked := make(chan struct{})
	ask := func(ctx context.Context, _ string, _ []string, _ bool, _ []string) (string, error) {
		close(asked)
		select {
		case a := <-answers:
			return a, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	d := withLiftedQueue(withAsk(nil, ask, nil), h)
	got := make(chan string, 1)
	go func() {
		out, _ := d(context.Background(), peerCall("ask_user",
			`{"question":"which?","options":["a","b"],"recommended":"a"}`))
		got <- out
	}()
	<-asked
	h.PeerMessage("x", "dun:1", "a", true)
	select {
	case out := <-got:
		t.Fatalf("the ask returned on a peer message: %q", out)
	case <-time.After(200 * time.Millisecond):
	}
	answers <- "b"
	out := <-got
	if !strings.HasPrefix(out, "b") || !strings.Contains(out, "[dun msg from dun:1") {
		t.Fatalf("want the user's answer, then the peer message: %q", out)
	}
}

// dund's word about the service reaches the model labelled as dun's, and
// is news worth a turn.
func TestServiceNotice_QueuedAsDunsAndWakes(t *testing.T) {
	h := testHarness(t)
	h.ServiceNotice("dun is about to be upgraded")
	h.noteMu.Lock()
	q := append([]queued(nil), h.queue...)
	h.noteMu.Unlock()
	if len(q) != 1 || q[0].kind != queuedService || q[0].policy().prefix != "[dun] " || !q[0].policy().causesTurn {
		t.Fatalf("queued: %+v", q)
	}
	select {
	case <-h.wake:
	default:
		t.Error("an idle session should be woken for it")
	}
}
