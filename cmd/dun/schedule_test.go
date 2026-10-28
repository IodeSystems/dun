package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 30, 0, 0, time.Local)
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		in   string
		want time.Time
		n    int
	}{
		{"10m", now.Add(10 * time.Minute), 1},
		{"2h30m", now.Add(150 * time.Minute), 1},
		{"+45s text", now.Add(45 * time.Second), 1},
		{"15:00", day(27, 15, 0), 1},
		{"9:05", day(28, 9, 5), 1}, // passed today: tomorrow
		{"14:30", day(28, 14, 30), 1},
		{"tomorrow 9:00 stand-up", day(28, 9, 0), 2},
		{"Tomorrow 23:59", day(28, 23, 59), 2},
		{"2026-10-29T08:00:00Z", time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC), 1},
	} {
		got, n, err := parseWhen(strings.Fields(c.in), now)
		if err != nil || !got.Equal(c.want) || n != c.n {
			t.Errorf("%q: %v %d %v; want %v %d", c.in, got, n, err, c.want, c.n)
		}
	}
	for _, bad := range []string{"", "soon", "-5m", "0s", "25:00", "9:5", "tomorrow", "tomorrow noon", "12:60"} {
		if _, _, err := parseWhen(strings.Fields(bad), now); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCutFieldsKeepsTheRest(t *testing.T) {
	f, rest := cutFields("  10m   line one\n  line two  ", 1)
	if len(f) != 1 || f[0] != "10m" || rest != "line one\n  line two  " {
		t.Errorf("%q %q", f, rest)
	}
	if f, rest := cutFields("edit", 2); len(f) != 1 || rest != "" {
		t.Errorf("short: %q %q", f, rest)
	}
}

// fakeSched records what /schedule and /loop ask dund, answering with an
// entry built from the request.
type fakeSched struct {
	reqs []proto.SchedReq
	list []proto.SchedEntry
}

func (f *fakeSched) ask(_ context.Context, r proto.SchedReq) (proto.SchedReply, error) {
	f.reqs = append(f.reqs, r)
	e := proto.SchedEntry{ID: max(r.ID, 7), Session: "me", Name: "dun:140000", At: r.At, Every: r.Every, Text: r.Text}
	if r.Action == "get" {
		e.Text = "the old text"
	}
	return proto.SchedReply{Entry: &e, Entries: f.list, Self: "me"}, nil
}

func TestScheduleCommand(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.Local)
	f := &fakeSched{}
	run := func(action string) (string, string) { return schedCmd(context.Background(), f.ask, action, now) }
	last := func() proto.SchedReq { return f.reqs[len(f.reqs)-1] }

	msg, _ := run("10m check the build\n  and say how long it took")
	if r := last(); r.Action != "add" || !r.At.Equal(now.Add(10*time.Minute)) || r.Text != "check the build\n  and say how long it took" || r.To != "" {
		t.Errorf("add: %+v", r)
	}
	if !strings.Contains(msg, "schedule 7: 14:10 (in 10m)") {
		t.Errorf("add said %q", msg)
	}
	run("tomorrow 9:00 --to dun:120000 stand-up notes")
	if r := last(); r.To != "dun:120000" || r.Text != "stand-up notes" || r.At.Day() != 28 {
		t.Errorf("--to: %+v", r)
	}
	run("push 7 +1h")
	if r := last(); r.Action != "push" || r.ID != 7 || r.Shift != time.Hour || !r.At.IsZero() {
		t.Errorf("push +1h: %+v", r)
	}
	run("push #7 16:00")
	if r := last(); r.Shift != 0 || !r.At.Equal(now.Add(2*time.Hour)) {
		t.Errorf("push 16:00: %+v", r)
	}
	run("edit 7 a new text")
	if r := last(); r.Action != "edit" || r.Text != "a new text" {
		t.Errorf("edit: %+v", r)
	}
	if _, edit := run("edit 7"); edit != "/schedule edit 7 the old text" || last().Action != "get" {
		t.Errorf("bare edit: composer %q, asked %+v", edit, last())
	}
	run("cancel 7")
	if r := last(); r.Action != "cancel" || r.ID != 7 {
		t.Errorf("cancel: %+v", r)
	}
	n := len(f.reqs)
	for _, bad := range []string{"soon do it", "10m", "push x 1h", "push 7 later"} {
		if msg, _ := run(bad); !strings.Contains(msg, "schedule") || len(f.reqs) != n {
			t.Errorf("%q: asked dund (%+v) or said %q", bad, f.reqs[n:], msg)
		}
	}
	f.list = []proto.SchedEntry{
		{ID: 2, Session: "me", Name: "dun:140000", At: now.Add(42 * time.Minute), Text: "first line\nsecond"},
		{ID: 3, Session: "me", Name: "dun:140000", Every: proto.SelfPaced, Iteration: 4, Awaiting: true, Text: "poll"},
	}
	msg, _ = run("")
	for _, want := range []string{"2  14:42 (in 42m)  → dun:140000", "first line", "3  running; next run when its turn ends", "loop, self-paced, 4 run(s)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("list lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "second") {
		t.Errorf("list shows more than the first line:\n%s", msg)
	}
}

func TestLoopCommand(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.Local)
	f := &fakeSched{}
	run := func(action string) string { return loopCmd(context.Background(), f.ask, action, now) }
	last := func() proto.SchedReq { return f.reqs[len(f.reqs)-1] }

	if msg := run("5m check the deploy"); !strings.Contains(msg, "every 5m") {
		t.Errorf("said %q", msg)
	}
	if r := last(); r.Action != "add" || r.Every != 5*time.Minute || r.Text != "check the deploy" || !r.At.Equal(now) {
		t.Errorf("fixed: %+v", r)
	}
	run("watch the queue")
	if r := last(); r.Every != proto.SelfPaced || r.Text != "watch the queue" {
		t.Errorf("self-paced: %+v", r)
	}
	run("stop the server when it is idle") // a prompt, not the verb
	if r := last(); r.Action != "add" || r.Text != "stop the server when it is idle" {
		t.Errorf("prompt starting with stop: %+v", r)
	}
	run("stop 9")
	if r := last(); r.Action != "cancel" || r.ID != 9 {
		t.Errorf("stop 9: %+v", r)
	}
	f.list = []proto.SchedEntry{{ID: 4, Session: "me", Every: proto.SelfPaced}, {ID: 5, Session: "other", Every: time.Hour}, {ID: 6, Session: "me"}}
	run("stop")
	if r := last(); r.Action != "cancel" || r.ID != 4 {
		t.Errorf("bare stop with one loop here: %+v", r)
	}
	n := len(f.reqs)
	if msg := run("30s too fast"); !strings.Contains(msg, "1m to 24h") || len(f.reqs) != n {
		t.Errorf("30s: %q", msg)
	}
	if msg := run(""); !strings.Contains(msg, "4  running") || strings.Contains(msg, "  6  ") {
		t.Errorf("bare /loop lists loops only:\n%s", msg)
	}
}

// /schedule and /loop reach the engine with their text as typed.
func TestRouteSlashKeepsScheduleText(t *testing.T) {
	var id, action string
	routeSlash("/schedule 10m Check  the\nBuild", slashRoutes{control: func(i, a string) { id, action = i, a }, reply: func(string) {}})
	if id != "schedule" || action != "10m Check  the\nBuild" {
		t.Errorf("%q %q", id, action)
	}
	routeSlash("/loop", slashRoutes{control: func(i, a string) { id, action = i, a }, reply: func(string) {}})
	if id != "loop" || action != "" {
		t.Errorf("bare loop: %q %q", id, action)
	}
}
