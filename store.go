package dun

import (
	"context"
	"sync"

	"github.com/iodesystems/agentkit/agent"
)

// memStore is a minimal in-memory agent.Store for a single dun session. Slice 1
// keeps the conversation in memory; durable persistence (resume, review history)
// is a later slice. Appended entries are visible to Context immediately;
// "pending" is the count of inbox arrivals not yet claimed.
type memStore struct {
	mu        sync.Mutex
	entries   []agent.Entry
	unclaimed int
}

func newMemStore() *memStore { return &memStore{} }

func (m *memStore) ClaimPending(_ context.Context, _ string, _ int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.unclaimed
	m.unclaimed = 0
	return n, nil
}

func (m *memStore) Append(_ context.Context, _ string, e agent.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memStore) Context(_ context.Context, _ string) ([]agent.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]agent.Entry, len(m.entries))
	copy(out, m.entries)
	return out, nil
}

func (m *memStore) Compact(_ context.Context, _ string, c agent.Compaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	subsumed := map[string]bool{}
	for _, e := range c.Subsumes {
		subsumed[e.ID] = true
	}
	kept := m.entries[:0:0]
	for _, e := range m.entries {
		if !subsumed[e.ID] {
			kept = append(kept, e)
		}
	}
	m.entries = append(kept, c.Marker)
	return nil
}

// publish appends an entry AND marks it a pending inbox arrival (a user message
// injected into the conversation).
func (m *memStore) publish(e agent.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	m.unclaimed++
}
