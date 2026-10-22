// Package dund is the session service: it runs each dun session's engine as
// a child process, numbers and keeps the session's events, and lets any
// number of clients — the TUI, a browser, a phone — attach to a session,
// send it commands and follow it live. Sessions outlive their clients.
//
// A session belongs to the principal that created it and is visible only to
// its members (the owner, for now). Each member keeps its own UI state per
// session, shared by that member's devices and by no one else.
package dund

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// createTimeout bounds how long a new engine may take to name its session.
// Starting tool servers takes seconds; a docker image pull can take longer.
const createTimeout = 3 * time.Minute

var (
	ErrNoSession = errors.New("no such session")
	ErrNotMember = errors.New("not a member of that session")
)

// Service is the session registry.
type Service struct {
	dir     string // registry and UI state live here
	spawner Spawner

	mu       sync.Mutex
	sessions map[string]*session
	closing  bool
	done     chan struct{} // closed by Close
}

// New opens the registry in dir, loading the sessions it lists. Their engines
// start on first use.
func New(dir string, spawner Spawner) (*Service, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dund: %w", err)
	}
	s := &Service{dir: dir, spawner: spawner, sessions: map[string]*session{}, done: make(chan struct{})}
	recs, err := s.loadRegistry()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		s.sessions[r.ID] = &session{svc: s, id: r.ID, workspace: r.Workspace, owner: r.Owner,
			members: r.Members, args: r.Args, log: proto.NewLog(r.ID, logCapacity)}
	}
	go s.reapIdle()
	return s, nil
}

// idleCheck is how often idle engines are looked for.
var idleCheck = time.Minute

// reapIdle stops engines that have sat idle for idleStop, until Close.
func (s *Service) reapIdle() {
	t := time.NewTicker(idleCheck)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.mu.Lock()
		var idle []*session
		for _, ses := range s.sessions {
			if ses.idle(idleStop) {
				idle = append(idle, ses)
			}
		}
		s.mu.Unlock()
		for _, ses := range idle {
			ses.log.Append(proto.NewEvent(&proto.EngineState{State: "stopping", Reason: "idle for " + idleStop.String()}))
			ses.stop()
		}
	}
}

func (s *Service) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Create starts an engine in workspace and registers the session it names,
// owned by principal.
//
// resume, when set, adopts that existing conversation; if the service already
// has it, its id comes back (to a member) and nothing is spawned.
func (s *Service) Create(principal, workspace string, args []string, resume string) (string, error) {
	if err := checkArgs(args); err != nil {
		return "", err
	}
	if resume != "" {
		if _, err := s.Get(principal, resume); err == nil {
			return resume, nil
		}
		s.mu.Lock()
		_, taken := s.sessions[resume]
		s.mu.Unlock()
		if taken {
			return "", ErrNoSession // someone else's
		}
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("workspace must be an absolute path: %q", workspace)
	}
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", workspace)
	}
	if s.isClosing() {
		return "", errors.New("service is shutting down")
	}
	spawnArgs := slices.Clone(args)
	if resume != "" {
		spawnArgs = append(spawnArgs, "--resume", resume)
	}
	eng, err := s.spawner.Spawn(workspace, spawnArgs)
	if err != nil {
		return "", err
	}
	// The engine names the session; everything before that is kept and
	// replayed into the session's log once it has a name.
	type named struct {
		id      string
		early   []proto.Event
		failure string
	}
	sc := newScanner(eng.Stdout())
	got := make(chan named, 1)
	go func() {
		var n named
		for sc.Scan() {
			ev, ok := decodeEvent(sc.Bytes())
			if !ok {
				continue
			}
			n.early = append(n.early, ev)
			if p, ok := ev.Payload.(*proto.Session); ok {
				n.id = p.ID
				break
			}
			if p, ok := ev.Payload.(*proto.Error); ok && p.Fatal {
				n.failure = p.Error
			}
		}
		got <- n
	}()
	var n named
	select {
	case n = <-got:
	case <-time.After(createTimeout):
		eng.Kill()
		return "", fmt.Errorf("engine did not start a session within %s", createTimeout)
	}
	if n.id == "" {
		eng.Kill()
		_ = eng.Wait()
		if n.failure != "" {
			return "", fmt.Errorf("engine failed to start: %s", n.failure)
		}
		return "", errors.New("engine exited before naming its session")
	}
	ses := &session{svc: s, id: n.id, workspace: workspace, owner: principal,
		members: []string{principal}, args: slices.Clone(args), log: proto.NewLog(n.id, logCapacity)}
	ses.mu.Lock()
	for _, ev := range n.early {
		ses.log.Append(ev)
	}
	ses.attach(eng, sc)
	ses.mu.Unlock()

	s.mu.Lock()
	s.sessions[n.id] = ses
	s.mu.Unlock()
	return n.id, s.saveRegistry()
}

// Get is a session principal may use.
func (s *Service) Get(principal, id string) (*session, error) {
	s.mu.Lock()
	ses, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNoSession
	}
	if !ses.isMember(principal) {
		// Not found, not forbidden: whether a session exists is itself only
		// for its members to know.
		return nil, ErrNoSession
	}
	return ses, nil
}

// List is principal's sessions, newest id last.
func (s *Service) List(principal string) []proto.SessionSummary {
	s.mu.Lock()
	var mine []*session
	for _, ses := range s.sessions {
		if ses.isMember(principal) {
			mine = append(mine, ses)
		}
	}
	s.mu.Unlock()
	out := make([]proto.SessionSummary, 0, len(mine))
	for _, ses := range mine {
		out = append(out, ses.summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Send routes a command to a session's engine, starting it if need be.
func (s *Service) Send(principal, id string, c proto.Command) error {
	ses, err := s.Get(principal, id)
	if err != nil {
		return err
	}
	return ses.send(c)
}

// Poll returns the session's events after seq after — waiting up to wait for
// the first one when there are none yet — at most max of them. A caller too
// far behind gets a snapshot first. For clients without a live stream.
func (s *Service) Poll(principal, id string, after uint64, wait time.Duration, max int) (*proto.Event, []proto.Event, error) {
	ses, err := s.Get(principal, id)
	if err != nil {
		return nil, nil, err
	}
	snap, replay, sub, err := ses.subscribe(after)
	if err != nil {
		return nil, nil, err
	}
	defer sub.Close()
	if err := ses.ensureRunning(); err != nil {
		return nil, nil, err
	}
	if len(replay) == 0 && snap == nil && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case ev, ok := <-sub.C():
			if ok {
				replay = append(replay, ev)
			}
		case <-t.C:
		}
		// What arrived with it (a turn is a burst) goes in the same answer.
	drain:
		for {
			select {
			case ev, ok := <-sub.C():
				if !ok {
					break drain
				}
				replay = append(replay, ev)
			default:
				break drain
			}
		}
	}
	if max > 0 && len(replay) > max {
		replay = replay[:max]
	}
	return snap, replay, nil
}

// Stream follows a session from after seq after: a snapshot first when the
// caller is too far behind, then the kept events, then live ones, on one
// channel that closes when ctx ends or the subscriber falls behind (resume
// from the last seq received). The engine is started if it is not running.
func (s *Service) Stream(ctx context.Context, principal, id string, after uint64) (<-chan proto.Event, error) {
	ses, err := s.Get(principal, id)
	if err != nil {
		return nil, err
	}
	snap, replay, sub, err := ses.subscribe(after)
	if err != nil {
		return nil, err
	}
	if err := ses.ensureRunning(); err != nil {
		sub.Close()
		return nil, err
	}
	out := make(chan proto.Event)
	go func() {
		defer close(out)
		defer sub.Close()
		send := func(ev proto.Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if snap != nil && !send(*snap) {
			return
		}
		for _, ev := range replay {
			if !send(ev) {
				return
			}
		}
		for {
			select {
			case ev, ok := <-sub.C():
				if !ok || !send(ev) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Restart restarts a session's engine with new flags, on the same
// conversation.
func (s *Service) Restart(principal, id string, args []string) error {
	if err := checkArgs(args); err != nil {
		return err
	}
	ses, err := s.Get(principal, id)
	if err != nil {
		return err
	}
	ses.stop()
	ses.mu.Lock()
	ses.args = slices.Clone(args)
	ses.mu.Unlock()
	if err := s.saveRegistry(); err != nil {
		return err
	}
	return ses.ensureRunning()
}

// Forget stops a session's engine and removes the session: registry entry,
// log, every member's UI state.
func (s *Service) Forget(principal, id string) error {
	ses, err := s.Get(principal, id)
	if err != nil {
		return err
	}
	ses.stop()
	ses.log.Close()
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.dir, "ui", id)); err != nil {
		return err
	}
	return s.saveRegistry()
}

// Stop ends a session's engine; the session stays registered.
func (s *Service) Stop(principal, id string) error {
	ses, err := s.Get(principal, id)
	if err != nil {
		return err
	}
	ses.stop()
	return nil
}

// Busy reports whether any session's engine is doing something a restart
// would cut short.
func (s *Service) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ses := range s.sessions {
		if ses.busy() {
			return true
		}
	}
	return false
}

// Close stops every engine. The registry keeps the sessions, so the next
// service picks them up.
func (s *Service) Close() {
	s.mu.Lock()
	if !s.closing {
		close(s.done)
	}
	s.closing = true
	all := make([]*session, 0, len(s.sessions))
	for _, ses := range s.sessions {
		all = append(all, ses)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ses := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ses.stop()
			ses.log.Close()
		}()
	}
	wg.Wait()
}

// --- the registry on disk ---

type record struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	Owner     string   `json:"owner"`
	Members   []string `json:"members"`
	Args      []string `json:"args,omitempty"`
}

func (s *Service) registryPath() string { return filepath.Join(s.dir, "sessions.json") }

func (s *Service) loadRegistry() ([]record, error) {
	b, err := os.ReadFile(s.registryPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dund: registry: %w", err)
	}
	var recs []record
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("dund: registry %s: %w", s.registryPath(), err)
	}
	return recs, nil
}

func (s *Service) saveRegistry() error {
	s.mu.Lock()
	recs := make([]record, 0, len(s.sessions))
	for _, ses := range s.sessions {
		recs = append(recs, record{ID: ses.id, Workspace: ses.workspace, Owner: ses.owner,
			Members: ses.members, Args: ses.args})
	}
	s.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.registryPath(), b)
}

// --- per-member UI state ---

func (s *Service) uiPath(id, principal string) string {
	sum := sha256.Sum256([]byte(principal))
	return filepath.Join(s.dir, "ui", id, hex.EncodeToString(sum[:8])+".json")
}

// UI is principal's UI state in session id; empty when none was saved.
func (s *Service) UI(principal, id string) (map[string]any, error) {
	if _, err := s.Get(principal, id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.uiPath(id, principal))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dund: ui state: %w", err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("dund: ui state: %w", err)
	}
	return st, nil
}

// PutUI replaces principal's UI state in session id.
func (s *Service) PutUI(principal, id string, state map[string]any) error {
	if _, err := s.Get(principal, id); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	p := s.uiPath(id, principal)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
