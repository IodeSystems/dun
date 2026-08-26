package dun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
			s.entries = append(s.entries, sanitizeOnLoad(e))
		}
		s.entries = pairToolResults(s.entries)
	}
	return s, nil
}

// pairToolResults re-kinds any tool RESULT whose call is not in the history.
//
// The mirror image of sanitizeOnLoad's problem, and produced by it: re-kinding a
// malformed call to a notification (deliberately clearing its id) leaves the
// result behind as a `role:"tool"` message referencing a tool_call_id no
// assistant message announces — which providers reject exactly as hard as the
// poison it was rescuing the session from. A turn that dies between a call and
// its result is the other source, and dun's Harness heals that one by writing the
// missing result; this direction has no call to write, so the result becomes text.
//
// Content is preserved either way: it is real tool output the model can still use.
func pairToolResults(entries []agent.Entry) []agent.Entry {
	announced := map[string]bool{}
	for _, e := range entries {
		if e.Kind == agent.KindToolCall && e.ToolCallID != "" {
			announced[e.ToolCallID] = true
		}
	}
	for i, e := range entries {
		if e.Kind != agent.KindToolResult || e.ToolCallID == "" || announced[e.ToolCallID] {
			continue
		}
		name := e.ToolName
		if name == "" {
			name = "a tool"
		}
		entries[i] = agent.Entry{
			ID:   e.ID,
			Kind: agent.KindNotification,
			Content: fmt.Sprintf("[recovered] Output from an earlier %s call whose request is no longer "+
				"in the history:\n\n%s", name, e.Content),
			Origin:    e.Origin,
			CreatedAt: e.CreatedAt,
		}
	}
	return entries
}

// sanitizeOnLoad rescues a session poisoned by a malformed tool call.
//
// A KindToolCall whose Content is not valid JSON makes the session
// UNRESUMABLE: providers deserialize historical tool_calls to render them (the
// Qwen chat template iterates `arguments` as a mapping to emit <parameter=…>
// tags), so every request carrying that entry is rejected at parse time, before
// the model is ever reached. Measured on a real session: 19,310 chars of
// unterminated JSON, and from then on every request returned the same 500 in
// 50ms — immune to compacting the context 51x, because the poison rides in the
// history rather than the size.
//
// The content is NOT discarded. It is the model's own partial work and it is
// still intact on disk, so it is re-kinded as a notification: rendered as TEXT,
// never as arguments, so it cannot be deserialized and cannot poison anything —
// while the model can still read how far it got and continue from there.
//
// Sessions written before the append-time check (harness) are the reason this
// exists; new ones never store such an entry in the first place.
func sanitizeOnLoad(e agent.Entry) agent.Entry {
	if e.Kind != agent.KindToolCall || json.Valid([]byte(e.Content)) {
		return e
	}
	name := e.ToolName
	if name == "" {
		name = "a tool"
	}
	return agent.Entry{
		ID:   e.ID,
		Kind: agent.KindNotification,
		Content: fmt.Sprintf(
			"[recovered] An earlier call to %s never completed — its arguments were cut off "+
				"and are not valid JSON, so the call was never executed (%d characters were "+
				"produced). Continue from where it stopped, with SMALLER writes.\n\n"+
				"Tail of what it had written:\n```\n%s\n```",
			name, len(e.Content), salvageTail(e.Content)),
		// ToolCallID/ToolName deliberately cleared: with them set this would
		// still correlate as half of a tool exchange whose result never exists.
		Origin:    e.Origin,
		CreatedAt: e.CreatedAt,
	}
}

// Loaded reports how many entries were restored from an existing session.
func (s *sessionStore) Loaded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}



// tagDocs marks aggregated proactive-RAG notifications (see notify.go), routed
// to the structured onDocs UI path instead of the plain onNotify.
const tagDocs = "docs"

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
	// Docs notifications drive a structured UI path (onDocs); skip the plain ping.
	if e.Kind == agent.KindNotification && e.Tag != tagDocs && cb != nil {
		cb(e.Content)
	}
	return nil
}

func (s *sessionStore) Context(_ context.Context, _ string) ([]agent.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		// A recap citation is a note to whoever reads the log, not conversation:
		// it is persisted, and it never reaches the model or the scrollback. A
		// pointer to removed churn that itself cost context would be absurd.
		// A recorded turn error is the same shape of thing for the same reason
		// (see kindTurnError).
		if e.Kind == kindRecap || e.Kind == kindTurnError {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// recap replaces a span of the conversation with one entry, records a citation,
// and optionally rewrites the anchoring user message.
//
// Built on the same primitive as Compact — drop a set, insert a marker, rewrite
// the file atomically — because it is the same operation with a different
// author: the model writes this summary instead of a summarizer LLM.
func (s *sessionStore) recap(subsumes []agent.Entry, replacement, citation agent.Entry, anchorID, userEdit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := map[string]bool{}
	for _, e := range subsumes {
		drop[e.ID] = true
	}
	kept := s.entries[:0:0]
	inserted := false
	for _, e := range s.entries {
		if drop[e.ID] {
			// The replacement takes the position of the first entry it replaces,
			// so the conversation keeps its shape: what came before is still
			// before it, and the live tool call still follows.
			if !inserted {
				kept = append(kept, replacement, citation)
				inserted = true
			}
			continue
		}
		if userEdit != "" && e.ID == anchorID {
			e.Content = userEdit
		}
		kept = append(kept, e)
	}
	if !inserted {
		kept = append(kept, replacement, citation)
	}
	s.entries = kept
	s.flushLocked()
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

// reRoot drops the given entries and inserts one marker at the FRONT of the
// history — the rescue variant of Compact, whose marker goes to the end. The
// folded prefix becomes "what happened earlier", so it must read BEFORE the
// surviving tail; a marker appended after it would float newer than the live
// conversation and reorder the tail behind its own summary.
func (s *sessionStore) reRoot(_ context.Context, subsumes []agent.Entry, marker agent.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := map[string]bool{}
	for _, e := range subsumes {
		drop[e.ID] = true
	}
	kept := s.entries[:0:0]
	for _, e := range s.entries {
		if !drop[e.ID] {
			kept = append(kept, e)
		}
	}
	s.entries = append([]agent.Entry{marker}, kept...)
	s.flushLocked()
	return nil
}

// dropTagged removes every entry carrying tag and reports how many went.
//
// The only deletion in this store that is not a fold. It exists for entries
// whose usefulness EXPIRES rather than being summarized: an overflow hint tells
// the model why its last response was cut off, and once a round completes
// without being cut, that is no longer true of anything. Leaving it in place
// costs context and, worse, keeps instructing the model to be brief long after
// the reason has gone.
//
// Nothing that carries WORK is ever dropped this way — see the tags it is called
// with, all of them machinery talking about itself.
func (s *sessionStore) dropTagged(tag string) int {
	if tag == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.entries[:0:0]
	dropped := 0
	for _, e := range s.entries {
		if e.Tag == tag {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	if dropped == 0 {
		return 0
	}
	s.entries = kept
	s.flushLocked()
	return dropped
}

// Reset clears all entries, starting a fresh session log.
func (s *sessionStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	s.unclaimed = 0
	s.flushLocked()
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

// appendSilent adds an entry WITHOUT marking it an inbox arrival and without
// the onNotify ping: the model reads it in the next turn's context, but its
// presence is not a reason to run one, and the UI already said its piece when
// the thing happened. This is what keeps a tool-set aside from costing a whole
// turn (see Harness.Aside).
func (s *sessionStore) appendSilent(e agent.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	s.flushLocked()
}

// pending reports how many inbox arrivals have not been claimed by a turn.
// A driver uses this to avoid running a turn with nothing new to say: an
// empty turn appends a second assistant message after the previous one, and
// a provider that rejects consecutive trailing assistant messages then fails
// the NEXT request ("cannot have 2 or more assistant messages at the end").
func (s *sessionStore) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unclaimed
}





func (s *sessionStore) notifyCallback() func(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onNotify
}

// salvageTailChars bounds how much of a cut-off tool call is re-injected. The
// whole entry rides in EVERY subsequent prompt, so preserving all of it would
// re-spend the context that the oversized write already cost — measured: a
// 19,310-char argument became a 19,545-char message on every turn.
//
// The TAIL is what matters: the model wrote the beginning and can see it in its
// own file; what it cannot infer is where it stopped.
const salvageTailChars = 1500

// salvageTail unwraps the argument envelope and returns the end of the payload.
//
// Raw arguments are a JSON object with escaped newlines
// ({"node":"…","newText":"package com…\n\n…"}), which is the wrong shape to hand
// back: the model would have to unescape its own file before continuing it.
// When the envelope is a truncated object with a string field, the field's text
// is unescaped and returned; otherwise the raw tail is, which is still better
// than nothing.
func salvageTail(args string) string {
	payload := args
	// The cut-off value is the LAST string field that never closed; take
	// everything after its opening quote.
	if i := strings.LastIndex(args, `":"`); i >= 0 {
		payload = args[i+3:]
	}
	payload = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(payload)
	if len(payload) > salvageTailChars {
		payload = "…" + payload[len(payload)-salvageTailChars:]
	}
	return payload
}
