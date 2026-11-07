package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

const testSettle = 60 * time.Millisecond

// startSettle starts a scripted root harness with a short settle delay.
func startSettle(t *testing.T, r *scriptRunner, sessionFile string, mut func(*Config)) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if sessionFile == "" {
		sessionFile = filepath.Join(dir, "s.jsonl")
	}
	cfg := Config{Workspace: dir, Servers: []Server{}, Client: r, SessionFile: sessionFile, SettleDelay: testSettle}
	if mut != nil {
		mut(&cfg)
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func settleArmed(h *Harness) bool {
	h.settle.mu.Lock()
	defer h.settle.mu.Unlock()
	return h.settle.armed
}

func settleQueued(h *Harness) int {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	n := 0
	for _, q := range h.queue {
		if q.tag == settleTag {
			n++
		}
	}
	return n
}

// A user turn with a tool call arms the timer; a turn that only answers does not.
func TestSettle_ArmsOnlyAfterToolTurn(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{textRound("hi"), toolRound("c1", "x"), textRound("done")}}
	h := startSettle(t, r, "", nil)
	if _, err := h.Ask(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if settleArmed(h) {
		t.Error("armed after a turn with no tool call")
	}
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if !settleArmed(h) {
		t.Error("not armed after a turn that used a tool")
	}
}

// A user message cancels the timer and takes back a queued prompt.
func TestSettle_UserMessageCancels(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done")}}
	h := startSettle(t, r, "", nil)
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	h.Say("one more thing")
	if settleArmed(h) {
		t.Fatal("Say left the timer armed")
	}
	time.Sleep(3 * testSettle)
	if n := settleQueued(h); n != 0 {
		t.Errorf("a cancelled timer queued %d prompts", n)
	}
	if _, err := os.Stat(settleFile(h.cfg.SessionFile)); err == nil {
		t.Error("the owed-settle sidecar survived the user's reply")
	}
}

// Presence restarts the delay: with pings the prompt comes later than without.
func TestSettle_PresenceRestartsTimer(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done")}}
	h := startSettle(t, r, "", func(c *Config) { c.SettleDelay = 150 * time.Millisecond })
	start := time.Now()
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		time.Sleep(70 * time.Millisecond)
		h.SettlePresence()
	}
	if settleQueued(h) != 0 {
		t.Fatalf("fired %v after the turn, despite pings every 70ms of a 150ms delay", time.Since(start))
	}
	select {
	case <-h.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("never fired after the pings stopped")
	}
	if got := time.Since(start); got < 300*time.Millisecond {
		t.Errorf("fired at %v: the pings did not move it", got)
	}
}

// Firing queues exactly one prompt and wakes; the settle turn reads it, answers,
// and does not arm another.
func TestSettle_FiresOnceAndDoesNotRearm(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done"), toolRound("c2", "x"), textRound("status: done")}}
	h := startSettle(t, r, "", nil)
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("no wake")
	}
	if n := settleQueued(h); n != 1 {
		t.Fatalf("queued %d settle prompts, want 1", n)
	}
	if h.Pending() == 0 {
		t.Error("the prompt would not start a turn")
	}
	res, err := h.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "status: done" {
		t.Errorf("reply %q", res.Reply)
	}
	r.mu.Lock()
	sent := sentText(r.sent[2])
	r.mu.Unlock()
	if !strings.Contains(sent, "[settle] The user has been away") {
		t.Errorf("the settle turn's request lacks the prompt:\n%s", sent)
	}
	// That turn used a tool too; it must not arm.
	time.Sleep(3 * testSettle)
	if settleArmed(h) || settleQueued(h) != 0 || len(h.Wake()) != 0 {
		t.Error("the settle turn armed another settle")
	}
	if _, err := os.Stat(settleFile(h.cfg.SessionFile)); err == nil {
		t.Error("the sidecar says a settle is owed after it finished")
	}
}

// Never on top of a running turn: the timer starts again instead.
func TestSettle_NotDuringATurn(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done")}}
	h := startSettle(t, r, "", nil)
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	h.settle.mu.Lock()
	h.settle.turns++ // a job's wake turn is running
	h.settle.mu.Unlock()
	time.Sleep(3 * testSettle)
	if settleQueued(h) != 0 || !settleArmed(h) {
		t.Fatalf("fired during a turn (queued %d, armed %v)", settleQueued(h), settleArmed(h))
	}
	h.settle.mu.Lock()
	h.settle.turns--
	h.settle.mu.Unlock()
	select {
	case <-h.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("did not fire once the turn ended")
	}
}

// Only the main agent, only interactive.
func TestSettle_NeverForChildOrNonInteractive(t *testing.T) {
	ask := func(h *Harness) {
		t.Helper()
		if _, err := h.Ask(context.Background(), "work"); err != nil {
			t.Fatal(err)
		}
	}
	tools := func() *scriptRunner {
		return &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done")}}
	}
	ni := startSettle(t, tools(), "", func(c *Config) { c.NonInteractive = true })
	ask(ni)
	if settleArmed(ni) {
		t.Error("armed under --non-interactive")
	}
	child := startSettle(t, tools(), "", nil)
	child.parent = &Harness{}
	ask(child)
	if settleArmed(child) {
		t.Error("armed for a child")
	}
	off := startSettle(t, tools(), "", func(c *Config) { c.SettleDelay = -1 })
	ask(off)
	if settleArmed(off) {
		t.Error("armed with the delay off")
	}
}

func TestSettleDelay_FlagEnvDefault(t *testing.T) {
	t.Setenv("DUN_SETTLE_DELAY", "")
	h := &Harness{}
	if got := h.settleDelay(); got != 2*time.Minute {
		t.Errorf("default %v", got)
	}
	t.Setenv("DUN_SETTLE_DELAY", "45s")
	if got := h.settleDelay(); got != 45*time.Second {
		t.Errorf("env %v", got)
	}
	t.Setenv("DUN_SETTLE_DELAY", "0")
	if got := h.settleDelay(); got != 0 {
		t.Errorf("env 0 %v", got)
	}
	h.cfg.SettleDelay = 5 * time.Second
	if got := h.settleDelay(); got != 5*time.Second {
		t.Errorf("config %v", got)
	}
	h.cfg.SettleDelay = -1
	if got := h.settleDelay(); got != 0 {
		t.Errorf("off %v", got)
	}
}

// The settle prompt leaves the history at the next user turn; the settle
// turn's own reply stays.
func TestSettle_PromptDroppedReplyStays(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done"), textRound("status: all done")}}
	h := startSettle(t, r, "", nil)
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	<-h.Wake()
	if _, err := h.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	has := func(f func(agent.Entry) bool) bool {
		for _, e := range entries(t, h) {
			if f(e) {
				return true
			}
		}
		return false
	}
	if !has(func(e agent.Entry) bool { return e.Tag == settleTag }) {
		t.Fatal("the prompt is not in the history during its own turn")
	}
	if _, err := h.Ask(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if has(func(e agent.Entry) bool { return e.Tag == settleTag || strings.Contains(e.Content, "has been away") }) {
		t.Error("the settle prompt survived the next user turn")
	}
	if !has(func(e agent.Entry) bool { return e.Kind == agent.KindAssistant && e.Content == "status: all done" }) {
		t.Error("the settle report was dropped with its prompt")
	}
}

// A settle owed when the session stopped: the first user turn after the
// resume carries one line, once, and costs no request of its own.
func TestSettle_OwedAcrossResume(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	r1 := &scriptRunner{rounds: []roundFn{toolRound("c1", "x"), textRound("done")}}
	h1 := startSettle(t, r1, sf, func(c *Config) { c.SettleDelay = time.Hour })
	if _, err := h1.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	h1.Close()
	if _, err := os.Stat(settleFile(sf)); err != nil {
		t.Fatalf("no sidecar after stopping with a settle owed: %v", err)
	}

	r2 := &scriptRunner{}
	h2 := startSettle(t, r2, sf, func(c *Config) { c.SettleDelay = time.Hour })
	if _, err := h2.Ask(context.Background(), "back again"); err != nil {
		t.Fatal(err)
	}
	if n := r2.calls(); n != 1 {
		t.Errorf("the note cost %d requests, want the turn's one", n)
	}
	r2.mu.Lock()
	first := sentText(r2.sent[0])
	r2.mu.Unlock()
	if strings.Count(first, settleResumeNote) != 1 {
		t.Errorf("the first turn has the note %d times:\n%s", strings.Count(first, settleResumeNote), first)
	}
	if _, err := h2.Ask(context.Background(), "and again"); err != nil {
		t.Fatal(err)
	}
	// The note is queued once: nothing is owed after the first turn.
	if h2.settle.resumeOwed || h2.settle.resumeNow {
		t.Error("the note is still owed after the first turn")
	}
	if _, err := os.Stat(settleFile(sf)); err == nil {
		t.Error("the sidecar outlived the user's reply")
	}
}

// Two plan changes leave one queued "plan" aside with the latest board.
func TestPlanBoard_OneKeyedAside(t *testing.T) {
	h := planHarness(t, false)
	planCall(t, h, `{"action":"add","title":"parse the file","done_when":"x"}`)
	ctx := context.Background()
	h.queue = nil
	if _, err := h.PlanCmd(ctx, "edit 1 title Parse it"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PlanCmd(ctx, "edit 1 title Parse it fast"); err != nil {
		t.Fatal(err)
	}
	q := queuedTexts(h)
	if len(q) != 1 || !strings.HasPrefix(q[0], "[plan] ") || !strings.Contains(q[0], "Parse it fast") || strings.Contains(q[0], "Parse it\n") {
		t.Fatalf("queued: %q", q)
	}
	if h.queue[0].key != "plan" || h.queue[0].kind != queuedAside {
		t.Errorf("not a keyed aside: %+v", h.queue[0])
	}
}
