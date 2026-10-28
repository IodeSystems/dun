package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// /schedule and /loop, the engine's half (docs/scheduling.md). dund keeps
// and fires the entries (internal/dund/schedule.go); this parses what the
// person typed, asks dund over the peer link, and words the answer. Times
// are parsed HERE, in the engine, because "15:00" is the person's local
// time and the engine runs as the person; dund only ever sees instants.

// schedUsage and loopUsage are the commands' usage lines.
const (
	schedUsage = "usage: /schedule <when> [--to <session>] <text> | list | push <id> <when>|+<dur> | edit <id> [text] | cancel <id>" +
		" — when: 10m, 2h30m, 15:00, tomorrow 9:00, or an RFC3339 time"
	loopUsage = "usage: /loop [interval] <prompt> | /loop (lists) | /loop stop [id] — no interval: the model paces it with loop_ctl"
)

// noDund is the answer when the engine has no dund on its stdio (dun -p).
const noDund = "needs dund (the session service): a one-shot dun -p has nothing to keep a schedule after it exits"

// parseWhen reads a time from the front of fields: a duration from now
// ("10m", "2h30m", "+10m"), a clock time today or, when that has passed,
// tomorrow ("15:00", "9:05"), "tomorrow 9:00", or an RFC3339 time. It
// returns the time and how many fields it took. Local time zone.
func parseWhen(fields []string, now time.Time) (time.Time, int, error) {
	if len(fields) == 0 {
		return time.Time{}, 0, errors.New("no time given")
	}
	f := fields[0]
	if d, err := time.ParseDuration(strings.TrimPrefix(f, "+")); err == nil {
		if d <= 0 {
			return time.Time{}, 0, fmt.Errorf("%q is not in the future", f)
		}
		return now.Add(d), 1, nil
	}
	if strings.EqualFold(f, "tomorrow") {
		if len(fields) < 2 {
			return time.Time{}, 0, errors.New("tomorrow needs a time: tomorrow 9:00")
		}
		h, m, ok := clockOf(fields[1])
		if !ok {
			return time.Time{}, 0, fmt.Errorf("%q is not a time of day (9:00, 15:30)", fields[1])
		}
		y, mo, d := now.AddDate(0, 0, 1).Date()
		return time.Date(y, mo, d, h, m, 0, 0, now.Location()), 2, nil
	}
	if h, m, ok := clockOf(f); ok {
		y, mo, d := now.Date()
		t := time.Date(y, mo, d, h, m, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, 1, nil
	}
	if t, err := time.Parse(time.RFC3339, f); err == nil {
		return t, 1, nil
	}
	return time.Time{}, 0, fmt.Errorf("%q is not a time: 10m, 2h30m, 15:00, tomorrow 9:00, or 2026-10-28T09:00:00-07:00", f)
}

// clockOf reads "15:04" (or "9:04").
func clockOf(s string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(s, ":")
	if !found || len(ms) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// cutFields takes n whitespace-separated fields off the front of s and
// returns them with the rest as typed (its newlines and spacing kept: it is
// a message).
func cutFields(s string, n int) ([]string, string) {
	var out []string
	s = strings.TrimLeft(s, " \t\n")
	for range n {
		if s == "" {
			break
		}
		i := strings.IndexAny(s, " \t\n")
		if i < 0 {
			out, s = append(out, s), ""
			break
		}
		out, s = append(out, s[:i]), strings.TrimLeft(s[i:], " \t\n")
	}
	return out, s
}

// schedAsk is one scheduler operation through the link.
type schedAsk func(ctx context.Context, req proto.SchedReq) (proto.SchedReply, error)

// schedCmd carries out /schedule. action is the line after the command,
// as typed. edit, when set, is a line for the composer: /schedule edit <id>
// with no text puts the current text there to be changed.
func schedCmd(ctx context.Context, ask schedAsk, action string, now time.Time) (msg, edit string) {
	head, rest := cutFields(action, 1)
	verb := ""
	if len(head) == 1 {
		verb = strings.ToLower(head[0])
	}
	idArg := func() (int, string, error) {
		f, r := cutFields(rest, 1)
		if len(f) == 0 {
			return 0, "", errors.New("needs an entry id (/schedule lists them)")
		}
		id, err := strconv.Atoi(strings.TrimPrefix(f[0], "#"))
		if err != nil {
			return 0, "", fmt.Errorf("%q is not an entry id", f[0])
		}
		return id, r, nil
	}
	switch verb {
	case "", "list", "ls":
		if strings.TrimSpace(rest) != "" {
			return schedUsage, ""
		}
		rep, err := ask(ctx, proto.SchedReq{Action: "list"})
		if err != nil {
			return "schedule: " + err.Error(), ""
		}
		return schedListing(rep.Entries, now, false), ""
	case "push", "edit", "cancel", "rm":
		id, r, err := idArg()
		if err != nil {
			return "schedule " + verb + ": " + err.Error(), ""
		}
		req := proto.SchedReq{Action: verb, ID: id}
		switch verb {
		case "rm":
			req.Action = "cancel"
		case "push":
			f, _ := cutFields(r, 3)
			if len(f) > 0 && strings.HasPrefix(f[0], "+") {
				d, err := time.ParseDuration(f[0][1:])
				if err != nil || d <= 0 {
					return fmt.Sprintf("schedule push: %q is not a duration such as +1h", f[0]), ""
				}
				req.Shift = d
				break
			}
			at, n, err := parseWhen(f, now)
			if err != nil {
				return "schedule push: " + err.Error(), ""
			}
			if n != len(f) {
				return schedUsage, ""
			}
			req.At = at
		case "edit":
			if strings.TrimSpace(r) == "" {
				rep, err := ask(ctx, proto.SchedReq{Action: "get", ID: id})
				if err != nil {
					return "schedule edit: " + err.Error(), ""
				}
				return fmt.Sprintf("schedule %d: %s\n(/schedule edit %d <text> replaces it)", id, rep.Entry.Text, id),
					fmt.Sprintf("/schedule edit %d %s", id, rep.Entry.Text)
			}
			req.Text = r
		}
		rep, err := ask(ctx, req)
		if err != nil {
			return "schedule " + verb + ": " + err.Error(), ""
		}
		e := *rep.Entry
		switch req.Action {
		case "cancel":
			return fmt.Sprintf("schedule %d cancelled: %s", e.ID, schedLine(e.Text, 60)), ""
		case "edit":
			return fmt.Sprintf("schedule %d: text replaced — %s", e.ID, schedLine(e.Text, 60)), ""
		}
		return fmt.Sprintf("schedule %d: now %s", e.ID, whenShown(e, now)), ""
	}
	// /schedule <when> [--to <session>] <text>
	f, _ := cutFields(action, 2)
	at, n, err := parseWhen(f, now)
	if err != nil {
		return "schedule: " + err.Error() + "\n" + schedUsage, ""
	}
	_, text := cutFields(action, n)
	to := ""
	if t, r := cutFields(text, 2); len(t) == 2 && t[0] == "--to" {
		to, text = t[1], r
	}
	if strings.TrimSpace(text) == "" {
		return "schedule: what should it say?\n" + schedUsage, ""
	}
	rep, err := ask(ctx, proto.SchedReq{Action: "add", At: at, To: to, Text: text})
	if err != nil {
		return "schedule: " + err.Error(), ""
	}
	e := *rep.Entry
	where := ""
	if to != "" {
		where = " to " + e.Name
	}
	return fmt.Sprintf("schedule %d%s: %s (/schedule push|edit|cancel %d)", e.ID, where, whenShown(e, now), e.ID), ""
}

// loopCmd carries out /loop: bare lists the loops, stop [id] ends one, and
// anything else starts one — its first run now, then every interval, or
// self-paced (no interval) by the model with loop_ctl.
func loopCmd(ctx context.Context, ask schedAsk, action string, now time.Time) string {
	f, rest := cutFields(action, 1)
	if len(f) == 0 {
		rep, err := ask(ctx, proto.SchedReq{Action: "list"})
		if err != nil {
			return "loop: " + err.Error()
		}
		return schedListing(rep.Entries, now, true)
	}
	// "stop" and "list" are verbs only alone or before an id: "/loop stop
	// the server if it is idle" is a prompt.
	if verb := strings.ToLower(f[0]); verb == "stop" || verb == "list" {
		g, more := cutFields(rest, 1)
		_, isID := idOf(g)
		if strings.TrimSpace(more) == "" && (len(g) == 0 || isID) {
			if verb == "list" {
				return loopCmd(ctx, ask, "", now)
			}
			return loopStop(ctx, ask, g, now)
		}
	}
	every := proto.SelfPaced
	text := action
	if d, err := time.ParseDuration(f[0]); err == nil {
		if d < time.Minute || d > 24*time.Hour {
			return "loop: an interval is 1m to 24h\n" + loopUsage
		}
		every, text = d, rest
	}
	if strings.TrimSpace(text) == "" {
		return "loop: what should it run?\n" + loopUsage
	}
	rep, err := ask(ctx, proto.SchedReq{Action: "add", At: now, Every: every, Text: text})
	if err != nil {
		return "loop: " + err.Error()
	}
	pace := "self-paced: the model sets each next run with loop_ctl"
	if every > 0 {
		pace = "every " + shortDur(every)
	}
	return fmt.Sprintf("loop %d: runs now, then %s (/loop stop %d)", rep.Entry.ID, pace, rep.Entry.ID)
}

func idOf(f []string) (int, bool) {
	if len(f) != 1 {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(f[0], "#"))
	return id, err == nil
}

// loopStop ends loop id, or this session's only loop.
func loopStop(ctx context.Context, ask schedAsk, f []string, now time.Time) string {
	id, ok := idOf(f)
	if !ok {
		rep, err := ask(ctx, proto.SchedReq{Action: "list"})
		if err != nil {
			return "loop stop: " + err.Error()
		}
		var mine []proto.SchedEntry
		for _, e := range rep.Entries {
			if e.Loop() && e.Session == rep.Self {
				mine = append(mine, e)
			}
		}
		switch len(mine) {
		case 0:
			return "loop stop: this session has no loop"
		case 1:
			id = mine[0].ID
		default:
			return "loop stop: which one? /loop stop <id>\n" + schedListing(mine, now, true)
		}
	}
	rep, err := ask(ctx, proto.SchedReq{Action: "cancel", ID: id})
	if err != nil {
		return "loop stop: " + err.Error()
	}
	return fmt.Sprintf("loop %d stopped after %d run(s): %s", id, rep.Entry.Iteration, schedLine(rep.Entry.Text, 60))
}

// schedListing is /schedule's (or, loops, /loop's) list.
func schedListing(es []proto.SchedEntry, now time.Time, loops bool) string {
	var lines []string
	for _, e := range es {
		if loops && !e.Loop() {
			continue
		}
		line := fmt.Sprintf("  %d  %s  → %s", e.ID, whenShown(e, now), e.Name)
		if e.Loop() {
			pace := "self-paced"
			if e.Every > 0 {
				pace = "every " + shortDur(e.Every)
			}
			line += fmt.Sprintf("  loop, %s, %d run(s)", pace, e.Iteration)
		}
		lines = append(lines, line+"\n      "+schedLine(e.Text, 80))
	}
	if len(lines) == 0 {
		if loops {
			return "no loops — " + loopUsage
		}
		return "nothing scheduled — " + schedUsage
	}
	head := "scheduled (/schedule push|edit|cancel <id>):"
	if loops {
		head = "loops (/loop stop <id>; /schedule push|edit|cancel <id> work on them too):"
	}
	return head + "\n" + strings.Join(lines, "\n")
}

// whenShown is an entry's next time, absolute and relative: "15:00 (in
// 42m)"; a self-paced loop in its turn has none yet.
func whenShown(e proto.SchedEntry, now time.Time) string {
	if e.At.IsZero() {
		return "running; next run when its turn ends (loop_ctl)"
	}
	abs := e.At.Local().Format("15:04")
	if y, m, d := e.At.Local().Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		abs = e.At.Local().Format("Mon Jan 2 15:04")
	}
	raw := e.At.Sub(now)
	rel := raw.Round(time.Minute)
	switch {
	case raw <= 0:
		return abs + " (due now)"
	case rel <= 0:
		return abs + " (in <1m)"
	}
	return abs + " (in " + shortDur(rel) + ")"
}

// schedLine is s's first line, clipped to n runes.
func schedLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// sched asks dund for one scheduler operation; the reply's loop count is
// this session's, and lists or drops loop_ctl.
func (p *peerLink) sched(ctx context.Context, req proto.SchedReq) (proto.SchedReply, error) {
	rep, err := p.call(ctx, proto.PeerRequest{Op: "sched", Sched: &req})
	if err != nil {
		return proto.SchedReply{}, err
	}
	if rep.Sched == nil {
		return proto.SchedReply{}, errors.New("dund answered without a schedule (an older dund? restart it: /upgrade)")
	}
	return *rep.Sched, nil
}

// LoopCtl is loop_ctl through dund. The reason is the person's to read: it
// goes out as a line in the session, beside the tool call.
func (p *peerLink) LoopCtl(ctx context.Context, nextIn time.Duration, stop bool, reason string) (dun.LoopState, error) {
	rep, err := p.sched(ctx, proto.SchedReq{Action: "loop_ctl", NextIn: nextIn, Stop: stop, Reason: reason})
	if err != nil {
		return dun.LoopState{}, err
	}
	e := rep.Entry
	st := dun.LoopState{ID: e.ID, Next: e.At, Stopped: stop}
	what := fmt.Sprintf("loop %d: next run %s", e.ID, whenShown(*e, time.Now()))
	if stop {
		what = fmt.Sprintf("loop %d stopped by the model", e.ID)
	}
	if r := strings.TrimSpace(reason); r != "" {
		what += " — " + r
	}
	p.em.emit(&proto.Notification{Kind: "schedule", Text: what})
	return st, nil
}

// shortDur is d without its zero units: "20m", "1h30m" (as dund words it).
func shortDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
