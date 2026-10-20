package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/agent"
)

// Re-heading — stub the older copy of a read the session made again.
//
// Measured over dun's own session logs (tools/rereads.py, 2026-10-20, 99
// sessions, 3,530 read calls): 42% of read calls fetched the EXACT same
// target the session had already read, and those repeats were 46% of all
// read bytes — 9.5 MB of 20.7 MB. node_read alone repeated 60% of the time.
// Some repeats follow an edit, so the content differs; either way the older
// copy is no longer what the model should act on, and it rides every round.
//
// The newest copy stays whole. Every older copy of the same target becomes a
// one-line stub naming the target, and the original goes to a sidecar beside
// the session file — the same rule recap follows: nothing is destroyed, and a
// sidecar that cannot be written aborts the re-heading.
//
// When: agent.Shaper.Rehead, which runs once per build only when the prompt is
// still over budget after LOD, immediately before a fold. A rewrite of earlier
// context breaks the provider's prompt cache from that entry on; at fold time
// the prefix is rewritten anyway, so both rewrites cost one cache break. (aw4
// built the same thing on the same hook; ak 90c08d3.)

// reheadMinChars is the smallest result worth stubbing: below it the stub
// saves little and the edit still costs a cache break at that position.
const reheadMinChars = 1000

const reheadStubPrefix = "[superseded — "

// readCommand matches an exec that only reads one thing: a single read
// command, no pipeline, no chaining, no redirection. Anything else may have
// side effects or depend on state, so it is never treated as a repeat.
var readCommand = regexp.MustCompile(`^\s*(cat|head|tail|nl|sed\s+-n)\s[^;&|<>` + "`" + `$()]*$`)

// readKey identifies what a tool call read, or "" when it is not a plain read.
func readKey(call agent.Entry) string {
	var args map[string]any
	if json.Unmarshal([]byte(call.Content), &args) != nil {
		return ""
	}
	switch call.ToolName {
	case "node_read":
		if n, _ := args["node"].(string); n != "" {
			return "node_read " + n
		}
	case "exec":
		if c, _ := args["command"].(string); readCommand.MatchString(c) {
			return "exec " + strings.TrimSpace(c)
		}
	}
	return ""
}

// supersededReads returns, for each older copy of a repeated read, its entry
// ID and the key it repeats. The newest copy of each key is never included.
func supersededReads(entries []agent.Entry) map[string]string {
	calls := map[string]agent.Entry{}
	type hit struct{ id, key string }
	var reads []hit
	last := map[string]string{} // key → ID of its newest result
	for _, e := range entries {
		switch e.Kind {
		case agent.KindToolCall:
			calls[e.ToolCallID] = e
		case agent.KindToolResult:
			c, ok := calls[e.ToolCallID]
			if !ok {
				continue
			}
			k := readKey(c)
			if k == "" {
				continue
			}
			last[k] = e.ID
			if len(e.Content) >= reheadMinChars && !strings.HasPrefix(e.Content, reheadStubPrefix) {
				reads = append(reads, hit{e.ID, k})
			}
		}
	}
	out := map[string]string{}
	for _, r := range reads {
		if last[r.key] != r.id {
			out[r.id] = r.key
		}
	}
	return out
}

// rehead is the Shaper's Rehead hook. Reports whether it changed the context.
func (h *Harness) rehead(ctx context.Context, sessionID string) bool {
	entries, err := h.store.Context(ctx, sessionID)
	if err != nil {
		return false
	}
	sup := supersededReads(entries)
	if len(sup) == 0 {
		return false
	}
	var originals []agent.Entry
	for _, e := range entries {
		if _, ok := sup[e.ID]; ok {
			originals = append(originals, e)
		}
	}
	side := h.nextSidecar("rehead")
	if h.cfg.SessionFile != "" {
		if side == "" || writeSidecar(side, originals) != nil {
			log.Printf("dun: re-heading skipped: could not archive %d superseded reads", len(originals))
			return false
		}
	}
	stubs := make(map[string]string, len(sup))
	saved := 0
	for _, e := range originals {
		stub := fmt.Sprintf("%s%s was read again later in this session; the newer result is current. Original archived in %s.]",
			reheadStubPrefix, sup[e.ID], sidecarName(side))
		stubs[e.ID] = stub
		saved += len(e.Content) - len(stub)
	}
	n := h.store.replaceContents(stubs)
	h.cache.noteRehead(n, saved)
	log.Printf("dun: re-headed %d superseded reads (%d chars) before the fold; originals in %s",
		n, saved, sidecarName(side))
	return n > 0
}

// nextSidecar numbers a sidecar of the given kind beside the session file
// ("" for an in-memory session).
func (h *Harness) nextSidecar(kind string) string {
	base := h.cfg.SessionFile
	if base == "" {
		return ""
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	for n := 1; n <= 999; n++ {
		p := fmt.Sprintf("%s.%s%d.jsonl", stem, kind, n)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
	return ""
}
