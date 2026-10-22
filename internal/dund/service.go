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
}

// New opens the registry in dir, loading the sessions it lists. Their engines
// start on first use.
func New(dir string, spawner Spawner) (*Service, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dund: %w", err)
	}
	s := &Service{dir: dir, spawner: spawner, sessions: map[string]*session{}}
	recs, err := s.loadRegistry()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		s.sessions[r.ID] = &session{svc: s, id: r.ID, workspace: r.Workspace, owner: r.Owner,
			members: r.Members, args: r.Args, log: proto.NewLog(r.ID, logCapacity)}
	}
	return s, nil
}

func (s *Service) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Create starts an engine in workspace and registers the session it names,
// owned by principal.
func (s *Service) Create(principal, workspace string, args []string) (string, error) {
	if err := checkArgs(args); err != nil {
		return "", err
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
	eng, err := s.spawner.Spawn(workspace, args)
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

// Stop ends a session's engine; the session stays registered.
func (s *Service) Stop(principal, id string) error {
	ses, err := s.Get(principal, id)
	if err != nil {
		return err
	}
	ses.stop()
	return nil
}

// Close stops every engine. The registry keeps the sessions, so the next
// service picks them up.
func (s *Service) Close() {
	s.mu.Lock()
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
