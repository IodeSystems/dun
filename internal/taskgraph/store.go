package taskgraph

import (
	"context"
	"sync"
)

// Store is where a plan lives. The host gives the tool one; Update runs fn on
// the plan under a lock and keeps the change only if fn returns nil.
type Store interface {
	// View is a copy of the plan.
	View(ctx context.Context) (Graph, error)
	Update(ctx context.Context, fn func(g *Graph) error) error
}

// MemStore keeps a plan in memory, and hands it to Save after each kept
// change: nil Save is a session with no file.
type MemStore struct {
	mu sync.Mutex
	g  Graph
	// Save writes the plan out. If it fails the change is undone.
	Save func(g Graph) error
}

// NewMemStore starts a store from g.
func NewMemStore(g Graph, save func(Graph) error) *MemStore { return &MemStore{g: g, Save: save} }

func (s *MemStore) View(context.Context) (Graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.g.Clone(), nil
}

func (s *MemStore) Update(_ context.Context, fn func(g *Graph) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.g.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if s.Save != nil {
		if err := s.Save(next); err != nil {
			return err
		}
	}
	s.g = next
	return nil
}
