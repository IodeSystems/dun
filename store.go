package dun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
)

// sessionStore is dun's conversation store: the agent.Store the Turn loop needs,
// plus the inbox helpers. It keeps entries in memory and — when Path is set —
// mirrors them to a JSONL session file (one Entry per line), so a session can be
// RESUMED later. Sessions live under ~/.dun/sessions/<root>/<id>.jsonl, scoped
// by the workspace ROOT (see session.go), à la ~/.claude. Path "" = in-memory
// only (no persistence).
//
// The file is rewritten wholesale on each change — a coding session is hundreds
// of entries, not millions, so a full flush is sub-millisecond and avoids an
// op-log/replay dance (Compact removes entries, which append-only can't). The
// rewrite is atomic (temp file + rename), so a crash mid-write never tears the
// session — the "a/b" safety, via rename.
//
// Large entry contents (a node_read of a whole file, a big diff, verbose exec
// output) are EXTRACTED to content-addressed blobs (blobs/<sha>.blob) and the
// JSONL keeps only a ref — so the session stays lean and identical reads dedup
// by hash. Extraction is disk-only: in-memory entries always hold full content
// (the model sees everything), and load re-materializes refs back to content.
type sessionStore struct {
	mu        sync.Mutex
	entries   []agent.Entry
	unclaimed int
	path      string // "" = memory only
	onNotify  func(string)
}

// openSessionStore opens (loading any existing) session file at path; path ""
// is memory-only.
func openSessionStore(path string) (*sessionStore, error) {
	s := &sessionStore{path: path}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var e agent.Entry
			if dec.Decode(&e) != nil {
				break
			}
			if strings.HasPrefix(e.Content, blobMarker) {
				if full, ok := s.readBlob(e.Content); ok {
					e.Content = full
				} else {
					e.Content = "[dun: session blob missing]"
				}
			}
			s.entries = append(s.entries, e)
		}
	}
	return s, nil
}

// Loaded reports how many entries were restored from an existing session.
func (s *sessionStore) Loaded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *sessionStore) setOnNotify(f func(string)) { s.onNotify = f }

// blobMarker prefixes a ref where an entry's content was extracted to a blob.
// It leads with NUL so it can't collide with real tool output or prose.
const blobMarker = "\x00dun-blob:"

// blobThreshold: contents larger than this are extracted to a blob rather than
// inlined. Below it, a separate file costs more than it saves.
const blobThreshold = 8 << 10 // 8 KiB

func (s *sessionStore) blobsDir() string { return filepath.Join(filepath.Dir(s.path), "blobs") }

// writeBlob stores content by hash and returns its ref; content-addressed so an
// identical read is written once. ok=false → caller inlines the content instead.
func (s *sessionStore) writeBlob(content string) (ref string, ok bool) {
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:])
	if os.MkdirAll(s.blobsDir(), 0o755) != nil {
		return "", false
	}
	path := filepath.Join(s.blobsDir(), name+".blob")
	if _, err := os.Stat(path); err != nil { // write once
		tmp := path + ".tmp"
		if os.WriteFile(tmp, []byte(content), 0o644) != nil || os.Rename(tmp, path) != nil {
			return "", false
		}
	}
	return blobMarker + name, true
}

// readBlob resolves a ref back to its content; ok=false if the blob is gone.
func (s *sessionStore) readBlob(ref string) (string, bool) {
	name := strings.TrimPrefix(ref, blobMarker)
	data, err := os.ReadFile(filepath.Join(s.blobsDir(), name+".blob"))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// flushLocked rewrites the session file from entries, extracting oversized
// contents to blobs. The range copies each Entry, so swapping Content for a ref
// touches only the on-disk form — in-memory entries keep full content. Caller
// holds mu.
func (s *sessionStore) flushLocked() {
	if s.path == "" {
		return
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, e := range s.entries {
		if len(e.Content) > blobThreshold {
			if ref, ok := s.writeBlob(e.Content); ok {
				e.Content = ref
			}
		}
		_ = enc.Encode(e)
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b.Bytes(), 0o644) == nil {
		_ = os.Rename(tmp, s.path) // atomic replace
	}
}

// ── agent.Store ────────────────────────────────────────────────────

func (s *sessionStore) ClaimPending(_ context.Context, _ string, _ int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.unclaimed
	s.unclaimed = 0
	return n, nil
}

func (s *sessionStore) Append(_ context.Context, _ string, e agent.Entry) error {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.flushLocked()
	cb := s.onNotify
	s.mu.Unlock()
	if e.Kind == agent.KindNotification && cb != nil {
		cb(e.Content)
	}
	return nil
}

func (s *sessionStore) Context(_ context.Context, _ string) ([]agent.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Entry, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

func (s *sessionStore) Compact(_ context.Context, _ string, c agent.Compaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	subsumed := map[string]bool{}
	for _, e := range c.Subsumes {
		subsumed[e.ID] = true
	}
	kept := s.entries[:0:0]
	for _, e := range s.entries {
		if !subsumed[e.ID] {
			kept = append(kept, e)
		}
	}
	s.entries = append(kept, c.Marker)
	s.flushLocked()
	return nil
}

// ── inbox helpers ──────────────────────────────────────────────────

// publish appends an entry AND marks it a pending inbox arrival (a user message
// injected into the conversation).
func (s *sessionStore) publish(e agent.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	s.unclaimed++
	s.flushLocked()
}

// publishNotification injects a KindNotification into the inbox (pending) and
// fires onNotify. Used by proactive RAG + background-job completions.
func (s *sessionStore) publishNotification(e agent.Entry) {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.unclaimed++
	s.flushLocked()
	cb := s.onNotify
	s.mu.Unlock()
	if cb != nil {
		cb(e.Content)
	}
}
