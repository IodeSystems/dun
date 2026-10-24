package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// The first frame does not wait for the session. Live, a new engine took
// 54 s to name its session (poly-lsp indexing) and the pane stayed blank,
// because the attach ran before the program started. The real program, with
// an attach that does not answer: it draws, says what it waits for, and
// ctrl+c quits.
func TestFirstFrame_DrawsBeforeTheAttachAnswers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	old := attachFn
	attachFn = func(tuiOpts, resumeAt) (*dunProc, error) {
		<-release
		return nil, errAttachReleased
	}
	t.Cleanup(func() { attachFn = old })

	const w, h = 49, 21 // the owner's phone, in tmux
	m := newTUIModel(nil, "/ws")
	m.startingStart = time.Now()
	out := &syncBuf{}
	p := tea.NewProgram(m,
		tea.WithInput(nil), tea.WithOutput(out), tea.WithWindowSize(w, h),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithColorProfile(colorprofile.TrueColor),
		tea.WithoutSignalHandler())
	done := make(chan struct{})
	go func() {
		_, _ = p.Run()
		close(done)
	}()
	t0 := time.Now()
	e := newEmu(w, h)
	fed := 0
	var screen string
	for time.Since(t0) < 3*time.Second {
		b := out.bytes()
		if len(b) > fed {
			e.feed(t, b[fed:])
			fed = len(b)
		}
		var rows []string
		for i := 0; i < h; i++ {
			rows = append(rows, e.row(i))
		}
		screen = strings.Join(rows, "\n")
		if strings.Contains(screen, "starting session…") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(screen, "starting session…") {
		t.Fatalf("no first frame naming the wait within 3s:\n%s", screen)
	}
	if d := time.Since(t0); d > time.Second {
		t.Errorf("the first frame took %s", d)
	}
	if !bytes.Contains(out.bytes(), []byte("\x1b[?1049h")) {
		t.Error("the program did not enter the alt screen")
	}
	p.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctrl+c did not quit while the attach was waiting")
	}
}

type attachErr string

func (e attachErr) Error() string { return string(e) }

const errAttachReleased = attachErr("released")

// Before ready the status line says what is awaited: the session while
// there is no connection, then the engine, then the servers it names.
func TestStartingStatus_NamesTheWait(t *testing.T) {
	m := resized(newTUIModel(nil, "/ws"), 49, 21)
	m.startingStart = time.Now().Add(-34 * time.Second)
	if got := lastRow(m); !strings.Contains(got, "starting session… · 34s") {
		t.Fatalf("no session: %q", got)
	}
	m.proc = &dunProc{stdin: discardWC{}}
	if got := lastRow(m); !strings.Contains(got, "starting the engine…") {
		t.Fatalf("an engine, no ready: %q", got)
	}
}

func lastRow(m tuiModel) string {
	rows := strings.Split(stripANSI(m.View().Content), "\n")
	return rows[len(rows)-1]
}

// A message typed before ready is kept, shown, and sent at the settings
// that follow ready — not dropped as enter used to be.
func TestKeptMessage_SentAtReady(t *testing.T) {
	eng := &echoWC{}
	m := resized(newTUIModel(&dunProc{stdin: eng}, "/ws"), 80, 24)
	m = typeStr(m, "what is this repo")
	m = key(m, kEnter)
	if eng.buf.Len() != 0 {
		t.Fatalf("sent before ready: %q", eng.buf.String())
	}
	if !strings.Contains(convoText(m), "what is this repo"+keptLine) {
		t.Fatalf("the kept message is not shown:\n%s", convoText(m))
	}
	if got := lastRow(m); !strings.Contains(got, "1 message kept") {
		t.Fatalf("status: %q", got)
	}
	nm, _ := m.Update(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = nm.(tuiModel)
	nm, _ = m.Update(evMsg{"type": "settings", "model": "m", "url": "http://llm/v1"})
	m = nm.(tuiModel)
	if !strings.Contains(eng.buf.String(), `"content":"what is this repo"`) {
		t.Fatalf("not sent at ready: %q", eng.buf.String())
	}
	if strings.Contains(convoText(m), keptLine) {
		t.Fatalf("the kept line stayed after it was sent:\n%s", convoText(m))
	}
	if !m.busy {
		t.Fatal("the turn it starts is not shown")
	}
}

// With nothing logged in, a kept message is held for /login like any other.
func TestKeptMessage_HeldWithoutALogin(t *testing.T) {
	eng := &echoWC{}
	m := resized(newTUIModel(&dunProc{stdin: eng}, "/ws"), 80, 24)
	m = typeStr(m, "hi")
	m = key(m, kEnter)
	nm, _ := m.Update(evMsg{"type": "ready"})
	m = nm.(tuiModel)
	nm, _ = m.Update(evMsg{"type": "settings", "url": ""})
	m = nm.(tuiModel)
	if strings.Contains(eng.buf.String(), `"type":"user"`) {
		t.Fatalf("sent with no login: %q", eng.buf.String())
	}
	if m.heldMsg != "hi" {
		t.Fatalf("held %q, want hi", m.heldMsg)
	}
}

// A server still starting at ready is in the splash and on the status
// line; when it comes up in the background the splash row says so in
// place — no row added, nothing below it moved — and no notice is added.
func TestLateServer_JoinsWithoutBounce(t *testing.T) {
	v := newVtui(49, 21)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	servers := func(starting bool) []any {
		return []any{
			map[string]any{"id": "shell", "running": true, "tools": float64(1)},
			map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "auto": true, "running": !starting,
				"starting": starting, "startingSeconds": float64(3), "tools": float64(4)},
		}
	}
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": servers(true)})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	if !strings.Contains(stripANSI(convoText(v.m)), "servers        poly-lsp starting 3s · shell on\n") {
		t.Fatalf("the splash does not say it is starting:\n%s", convoText(v.m))
	}
	if got := lastRow(v.m); !strings.Contains(got, "ready · poly-lsp starting") {
		t.Fatalf("status: %q", got)
	}
	before := strings.Split(v.view(), "\n")
	n := len(v.m.convo)
	v.event(map[string]any{"type": "server", "id": "poly-lsp", "action": "start", "background": true,
		"message": "poly-lsp: running · 4 tools", "servers": servers(false), "tools": []any{"eval", "node_query"}})
	if len(v.m.convo) != n {
		t.Fatalf("a background start added a notice:\n%s", convoText(v.m))
	}
	after := strings.Split(v.view(), "\n")
	if len(after) != len(before) {
		t.Fatalf("the screen changed height: %d → %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	// The servers row, and the status line's tail.
	if changed > 2 {
		t.Fatalf("%d rows changed:\nbefore:\n%s\nafter:\n%s", changed, strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if strings.Contains(v.view(), "starting") {
		t.Fatalf("still says starting:\n%s", v.view())
	}
}
