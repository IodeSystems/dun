package dund

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The scheduler: messages a person set to arrive later (/schedule) and
// prompts that repeat (/loop), for their own sessions. docs/scheduling.md
// has the reasoning; this is the short form.
//
// It lives in dund, not in an engine or a client, because the two things it
// must survive are exactly the things those do not: no client attached (a
// message set for 9:00 tomorrow is set so the person can leave), and the
// engine reaped for being idle (30 minutes of nothing, which is what a
// session waiting for 9:00 tomorrow is). dund outlives both, and is what
// starts an engine again on a command, so firing is just a command: a user
// message through session.send, which resumes a stopped engine on the same
// conversation. A busy session takes it the way it takes a message typed
// mid-turn — the engine queues it into the running turn — because that is
// what a message is; dund adds no queue of its own.
//
// ONE goroutine, ONE timer, on the earliest entry. Entries are few (a person
// types each one), so every operation is a scan; an index would be a second
// thing to keep right. The timer is never trusted past schedMaxWait: a
// monotonic timer does not count time a laptop spent asleep, and a wall
// clock moved by NTP or a DST change is not seen by it either. Waking once
// a minute to read the wall clock costs nothing and makes "fires at 15:00"
// mean the wall clock's 15:00.
//
// A loop is a repeating entry, not another mechanism. Fixed loops (Every >
// 0) move At on by Every each firing; a self-paced loop (proto.SelfPaced)
// fires, then waits with no time at all (Awaiting) until its session's turn
// ends — by then the model has either called loop_ctl, which set the next
// time, or it has not, and selfPaceDefault applies and the person is told.
// Missed firings (dund was down) are not replayed as a burst: the next
// firing counts from when the late one actually went.

// Variables so tests can shorten them.
var (
	// selfPaceDefault is a self-paced loop's next run when the model ended
	// its turn without calling loop_ctl.
	selfPaceDefault = 20 * time.Minute
	// schedRetry is how long a firing waits when it cannot go now: a loop
	// whose session is in a turn (a loop prompt stacked on its own previous
	// run, still going, is the one message nobody wants), or an engine that
	// would not start.
	schedRetry = time.Minute
	// schedMaxWait bounds one sleep of the scheduler (see above).
	schedMaxWait = time.Minute
)

// The bounds on a loop's pace. A loop faster than a minute is a busy-wait
// that pays for a model call each spin; slower than a day is a reminder,
// which /schedule is for.
const (
	loopMin = time.Minute
	loopMax = 24 * time.Hour
)

// schedEntry is an entry as stored: what clients see, and the owner (whose
// entries a principal may touch) and when it last fired (which loop a
// loop_ctl call means).
type schedEntry struct {
	proto.SchedEntry
	Owner string    `json:"owner"`
	Fired time.Time `json:"fired,omitzero"`
}

type schedFile struct {
	Next    int           `json:"next"`
	Entries []*schedEntry `json:"entries"`
}

// scheduler holds the entries and fires them. It takes no other lock while
// holding its own: busy and deliver reach into sessions, and are only ever
// called with mu released.
type scheduler struct {
	path string
	now  func() time.Time
	// busy reports whether a session is in a turn; deliver sends one firing.
	// An error from deliver puts the firing back schedRetry later, unless
	// it is ErrNoSession (the session is gone, and so is the entry).
	busy    func(session string) bool
	deliver func(e schedEntry) error

	saveMu sync.Mutex // orders writes, as Service.saveMu does

	mu      sync.Mutex
	next    int
	entries map[int]*schedEntry
	kick    chan struct{}
}

func loadScheduler(path string, now func() time.Time) (*scheduler, error) {
	sc := &scheduler{path: path, now: now, entries: map[int]*schedEntry{}, kick: make(chan struct{}, 1),
		busy: func(string) bool { return false }, deliver: func(schedEntry) error { return nil }}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dund: schedule: %w", err)
	}
	var f schedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("dund: schedule %s: %w", path, err)
	}
	sc.next = f.Next
	for _, e := range f.Entries {
		// A self-paced loop that was waiting for its turn to end will not see
		// that end: the dund that started the turn is gone. Its default
		// applies, as if the turn had ended without loop_ctl.
		if e.Awaiting {
			e.Awaiting, e.At = false, now().Add(selfPaceDefault)
		}
		sc.entries[e.ID] = e
		sc.next = max(sc.next, e.ID)
	}
	return sc, nil
}

// save writes every entry, whole file to a temp and a rename.
func (sc *scheduler) save() {
	sc.saveMu.Lock()
	defer sc.saveMu.Unlock()
	sc.mu.Lock()
	f := schedFile{Next: sc.next, Entries: make([]*schedEntry, 0, len(sc.entries))}
	for _, e := range sc.entries {
		c := *e
		c.Name = "" // a display field, computed when listed
		f.Entries = append(f.Entries, &c)
	}
	sc.mu.Unlock()
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].ID < f.Entries[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = writeFileAtomic(sc.path, b)
	}
	if err != nil {
		log.Printf("dund: schedule: %v", err)
	}
}

// poke wakes the loop to look at the earliest entry again.
func (sc *scheduler) poke() {
	select {
	case sc.kick <- struct{}{}:
	default:
	}
}

// changed saves and pokes: every operation that moves or removes an entry.
func (sc *scheduler) changed() {
	sc.save()
	sc.poke()
}

// run fires entries as they fall due, until done closes.
func (sc *scheduler) run(done <-chan struct{}) {
	for {
		sc.fireDue()
		wait := schedMaxWait
		if at, ok := sc.earliest(); ok {
			wait = min(wait, max(0, at.Sub(sc.now())))
		}
		t := time.NewTimer(wait)
		select {
		case <-done:
			t.Stop()
			return
		case <-sc.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// earliest is the soonest time any entry fires.
func (sc *scheduler) earliest() (time.Time, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	var at time.Time
	for _, e := range sc.entries {
		if !e.At.IsZero() && (at.IsZero() || e.At.Before(at)) {
			at = e.At
		}
	}
	return at, !at.IsZero()
}

// fireDue fires every entry due now, earliest first (ties by id, which is
// the order they were made), and returns what it fired.
func (sc *scheduler) fireDue() []schedEntry {
	now := sc.now()
	sc.mu.Lock()
	var due []schedEntry
	for _, e := range sc.entries {
		if !e.At.IsZero() && !e.At.After(now) {
			due = append(due, *e)
		}
	}
	sc.mu.Unlock()
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].At.Equal(due[j].At) {
			return due[i].At.Before(due[j].At)
		}
		return due[i].ID < due[j].ID
	})
	// Asked with the lock released: busy takes the session's.
	busy := map[string]bool{}
	for _, d := range due {
		if d.Loop() {
			if _, ok := busy[d.Session]; !ok {
				busy[d.Session] = sc.busy(d.Session)
			}
		}
	}
	var fired []schedEntry
	sc.mu.Lock()
	for _, d := range due {
		e, ok := sc.entries[d.ID]
		if !ok || !e.At.Equal(d.At) { // cancelled or pushed meanwhile
			continue
		}
		if e.Loop() && busy[e.Session] {
			e.At = now.Add(schedRetry)
			continue
		}
		f := *e
		f.Fired = now
		switch {
		case e.Every == 0:
			delete(sc.entries, e.ID)
		case e.Every == proto.SelfPaced:
			e.Iteration++
			e.At, e.Awaiting, e.Fired = time.Time{}, true, now
			f.Iteration = e.Iteration
		default:
			e.Iteration++
			e.At, e.Fired = now.Add(e.Every), now
			f.Iteration = e.Iteration
		}
		fired = append(fired, f)
	}
	sc.mu.Unlock()
	sc.save()
	var retry []schedEntry
	for _, f := range fired {
		err := sc.deliver(f)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoSession):
			log.Printf("dund: schedule: entry %d: session %s is gone; dropped", f.ID, f.Session)
			sc.mu.Lock()
			delete(sc.entries, f.ID)
			sc.mu.Unlock()
		default:
			log.Printf("dund: schedule: entry %d to %s: %v; again in %s", f.ID, f.Session, err, schedRetry)
			retry = append(retry, f)
		}
	}
	if len(retry) > 0 {
		sc.mu.Lock()
		for _, f := range retry {
			f.At, f.Awaiting = now.Add(schedRetry), false
			if f.Loop() {
				f.Iteration-- // it did not run
			}
			c := f
			sc.entries[f.ID] = &c
		}
		sc.mu.Unlock()
	}
	sc.save()
	return fired
}

// turnEnded is a turn's end in session: a self-paced loop of it still
// waiting gets selfPaceDefault. It returns those, for the person to be told.
func (sc *scheduler) turnEnded(session string) []schedEntry {
	now := sc.now()
	var out []schedEntry
	sc.mu.Lock()
	for _, e := range sc.entries {
		if e.Session == session && e.Awaiting {
			e.Awaiting, e.At = false, now.Add(selfPaceDefault)
			out = append(out, *e)
		}
	}
	sc.mu.Unlock()
	if len(out) > 0 {
		sc.changed()
	}
	return out
}

var errNoEntry = errors.New("no such scheduled entry")

func noEntry(id int) error { return fmt.Errorf("%w: %d (/schedule lists them)", errNoEntry, id) }

// add stores e (its ID and Created are set here) and returns it.
func (sc *scheduler) add(e schedEntry) schedEntry {
	sc.mu.Lock()
	sc.next++
	e.ID, e.Created = sc.next, sc.now()
	c := e
	sc.entries[e.ID] = &c
	sc.mu.Unlock()
	sc.changed()
	return e
}

// list is owner's entries by when they fire; a waiting self-paced loop,
// which has no time yet, last.
func (sc *scheduler) list(owner string) []schedEntry {
	sc.mu.Lock()
	var out []schedEntry
	for _, e := range sc.entries {
		if e.Owner == owner {
			out = append(out, *e)
		}
	}
	sc.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.At.IsZero() != b.At.IsZero() {
			return b.At.IsZero()
		}
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.ID < b.ID
	})
	return out
}

// update runs f on owner's entry id under the lock, then saves; f returning
// false removes the entry. It returns the entry as f left it.
func (sc *scheduler) update(owner string, id int, f func(e *schedEntry) (keep bool, err error)) (schedEntry, error) {
	sc.mu.Lock()
	e, ok := sc.entries[id]
	if !ok || e.Owner != owner {
		sc.mu.Unlock()
		return schedEntry{}, noEntry(id)
	}
	keep, err := f(e)
	if err != nil {
		sc.mu.Unlock()
		return schedEntry{}, err
	}
	out := *e
	if !keep {
		delete(sc.entries, id)
	}
	sc.mu.Unlock()
	sc.changed()
	return out, nil
}

func (sc *scheduler) get(owner string, id int) (schedEntry, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	e, ok := sc.entries[id]
	if !ok || e.Owner != owner {
		return schedEntry{}, noEntry(id)
	}
	return *e, nil
}

// push moves entry id to at, or by shift past its current time (from now
// when it has none: a self-paced loop waiting for its turn).
func (sc *scheduler) push(owner string, id int, at time.Time, shift time.Duration) (schedEntry, error) {
	now := sc.now()
	return sc.update(owner, id, func(e *schedEntry) (bool, error) {
		if shift != 0 {
			at = e.At
			if at.IsZero() {
				at = now
			}
			at = at.Add(shift)
		}
		if at.IsZero() {
			return false, errors.New("push needs a time")
		}
		e.At, e.Awaiting = at, false
		return true, nil
	})
}

func (sc *scheduler) edit(owner string, id int, text string) (schedEntry, error) {
	if strings.TrimSpace(text) == "" {
		return schedEntry{}, errors.New("edit needs the new text")
	}
	return sc.update(owner, id, func(e *schedEntry) (bool, error) {
		e.Text = text
		return true, nil
	})
}

func (sc *scheduler) cancel(owner string, id int) (schedEntry, error) {
	return sc.update(owner, id, func(*schedEntry) (bool, error) { return false, nil })
}

// loopCtl is the model's loop_ctl in session: the loop of it that fired
// last stops, or fires next nextIn from now (clamped to loopMin..loopMax).
func (sc *scheduler) loopCtl(owner, session string, nextIn time.Duration, stop bool) (schedEntry, error) {
	sc.mu.Lock()
	var pick *schedEntry
	for _, e := range sc.entries {
		if e.Owner == owner && e.Session == session && e.Loop() &&
			(pick == nil || e.Fired.After(pick.Fired) || e.Fired.Equal(pick.Fired) && e.ID > pick.ID) {
			pick = e
		}
	}
	sc.mu.Unlock()
	if pick == nil {
		return schedEntry{}, errors.New("this session has no loop")
	}
	if !stop && nextIn <= 0 {
		return schedEntry{}, errors.New("loop_ctl needs next_in (a duration such as \"15m\") or stop:true")
	}
	nextIn = min(max(nextIn, loopMin), loopMax)
	now := sc.now()
	return sc.update(owner, pick.ID, func(e *schedEntry) (bool, error) {
		if stop {
			return false, nil
		}
		e.At, e.Awaiting = now.Add(nextIn), false
		return true, nil
	})
}

// loops is how many loops session has.
func (sc *scheduler) loops(session string) int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	n := 0
	for _, e := range sc.entries {
		if e.Session == session && e.Loop() {
			n++
		}
	}
	return n
}

// has reports whether anything is scheduled into session: a session that
// is waiting for a message must not be forgotten for having none yet.
func (sc *scheduler) has(session string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, e := range sc.entries {
		if e.Session == session {
			return true
		}
	}
	return false
}

// dropSession removes every entry into session (it was forgotten).
func (sc *scheduler) dropSession(session string) {
	sc.mu.Lock()
	n := len(sc.entries)
	for id, e := range sc.entries {
		if e.Session == session {
			delete(sc.entries, id)
		}
	}
	changed := len(sc.entries) != n
	sc.mu.Unlock()
	if changed {
		sc.changed()
	}
}

// --- the Service's half: who may do what, and what a firing says ---

// schedOp runs a scheduler operation for the engine of session from.
func (s *Service) schedOp(from *session, req proto.SchedReq) (proto.SchedReply, error) {
	sc, owner := s.sched, from.owner
	var e schedEntry
	var err error
	switch req.Action {
	case "add":
		e, err = s.schedAdd(from, req)
	case "list":
		return proto.SchedReply{Entries: s.schedShown(owner, sc.list(owner)), Self: from.id}, nil
	case "get":
		e, err = sc.get(owner, req.ID)
	case "push":
		e, err = sc.push(owner, req.ID, req.At, req.Shift)
	case "edit":
		e, err = sc.edit(owner, req.ID, req.Text)
	case "cancel":
		e, err = sc.cancel(owner, req.ID)
	case "loop_ctl":
		e, err = sc.loopCtl(owner, from.id, req.NextIn, req.Stop)
	default:
		err = fmt.Errorf("unknown schedule action %q", req.Action)
	}
	if err != nil {
		return proto.SchedReply{}, err
	}
	shown := s.schedShown(owner, []schedEntry{e})[0]
	return proto.SchedReply{Entry: &shown, Self: from.id}, nil
}

// schedAdd checks and stores a new entry from session from.
func (s *Service) schedAdd(from *session, req proto.SchedReq) (schedEntry, error) {
	switch {
	case strings.TrimSpace(req.Text) == "":
		return schedEntry{}, errors.New("nothing to send: the message is empty")
	case req.At.IsZero():
		return schedEntry{}, errors.New("no time to send it at")
	case req.Every != 0 && req.Every != proto.SelfPaced && (req.Every < loopMin || req.Every > loopMax):
		return schedEntry{}, fmt.Errorf("a loop's interval is %s to %s", loopMin, loopMax)
	}
	target := from.id
	if req.To != "" {
		id, err := s.resolveSession(from.owner, req.To)
		if err != nil {
			return schedEntry{}, err
		}
		target = id
	}
	names := s.sessionNames(from.owner)
	e := schedEntry{Owner: from.owner, SchedEntry: proto.SchedEntry{Session: target, At: req.At,
		Every: req.Every, Text: req.Text, By: from.id, ByName: names[from.id]}}
	return s.sched.add(e), nil
}

// sessionNames is every session of principal's by id → its name, as
// dun_list names them — over all of them, not only the loaded ones: a
// message may be meant for a session asleep until it arrives.
func (s *Service) sessionNames(principal string) map[string]string {
	var rows []proto.PeerSession
	for _, ses := range s.all() {
		if ses.isMember(principal) {
			rows = append(rows, proto.PeerSession{ID: ses.id, Workspace: ses.workspace})
		}
	}
	nameSessions(rows)
	out := map[string]string{}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// resolveSession is principal's session to (an id, or a name as dun_list
// or sessionNames gives it).
func (s *Service) resolveSession(principal, to string) (string, error) {
	to = strings.TrimSpace(to)
	if _, err := s.Get(principal, to); err == nil {
		return to, nil
	}
	var hit []string
	names := s.sessionNames(principal)
	for id, n := range names {
		if n == to {
			hit = append(hit, id)
		}
	}
	// dun_list names only the live sessions, which can name one less
	// ambiguously than the whole list does.
	if len(hit) == 0 {
		for _, p := range s.Peers(principal, "") {
			if p.Name == to {
				hit = append(hit, p.ID)
			}
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return "", fmt.Errorf("no session %q (/sessions, or dun_list, names them)", to)
	}
	slices.Sort(hit)
	return "", fmt.Errorf("%q names %d sessions — use an id: %s", to, len(hit), strings.Join(hit, ", "))
}

// schedShown is es with each target's name filled in.
func (s *Service) schedShown(owner string, es []schedEntry) []proto.SchedEntry {
	names := s.sessionNames(owner)
	out := make([]proto.SchedEntry, len(es))
	for i, e := range es {
		out[i] = e.SchedEntry
		out[i].Name = names[e.Session]
		if out[i].Name == "" {
			out[i].Name = e.Session
		}
	}
	return out
}

// deliverScheduled sends one firing to its session as a user message,
// starting the engine if it is not running. A loop's firing is preceded by
// the loop count, so loop_ctl is in the tool set for the turn the firing
// starts (the engine reads its stdin in order).
func (s *Service) deliverScheduled(e schedEntry) error {
	ses, err := s.Get(e.Owner, e.Session)
	if err != nil {
		return err
	}
	return ses.send(proto.NewCommand("", &proto.User{Content: scheduledText(e, e.Fired, e.By == e.Session)}))
}

// scheduledPrefix starts every message the scheduler delivers; clients
// show such a message as scheduled, not typed (cmd/dun userRowAt), the way
// resumePrefix marks dund's own.
const scheduledPrefix = "[scheduled"

// loopPrefix starts a loop's firing.
const loopPrefix = "[loop"

// scheduledText is what a firing says. One short bracket ahead of the
// person's text, because every first request is measured and a loop pays
// it each run: that it was set earlier (when, and from where if not here),
// and for a loop which run this is and that loop_ctl paces it. A firing
// more than a minute late says when it was due: the model should not take
// "the deploy at 15:00" to be now.
func scheduledText(e schedEntry, sent time.Time, here bool) string {
	var b strings.Builder
	if e.Loop() {
		fmt.Fprintf(&b, "%s %d, run %d", loopPrefix, e.ID, e.Iteration)
		if e.Every == proto.SelfPaced {
			fmt.Fprintf(&b, ", self-paced: loop_ctl sets the next run (else %s) or stops it]", shortDur(selfPaceDefault))
		} else {
			fmt.Fprintf(&b, ", every %s: loop_ctl can stop it]", shortDur(e.Every))
		}
	} else {
		fmt.Fprintf(&b, "%s %s", scheduledPrefix, clock(e.Created, sent))
		if !here {
			b.WriteString(" from session " + nameOr(e.ByName, e.By))
		}
		if late := sent.Sub(e.At); late > time.Minute {
			fmt.Fprintf(&b, ", due %s, sent %s", clock(e.At, sent), clock(sent, sent))
		} else {
			b.WriteString(" for " + clock(e.At, sent))
		}
		b.WriteString("]")
	}
	return b.String() + " " + e.Text
}

func nameOr(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// clock is t as a person reads it beside now: "15:04", or with the date
// when it is another day.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// shortDur is d without its zero units: "20m", "1h30m".
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

// loopTurnEnded is a turn's end in ses: a self-paced loop that fired into
// it and was not given a next time by loop_ctl gets the default, and the
// person is told in the session, since the model chose nothing.
func (s *Service) loopTurnEnded(ses *session) {
	if s.sched == nil {
		return
	}
	for _, e := range s.sched.turnEnded(ses.id) {
		ev := proto.NewEvent(&proto.Notification{Kind: "schedule", Text: fmt.Sprintf(
			"loop %d: loop_ctl was not called; next run in %s (%s)", e.ID, shortDur(selfPaceDefault), clock(e.At, time.Now()))})
		ev.TS = time.Now()
		ses.log.Append(ev)
	}
}
