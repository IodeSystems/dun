// Package replay drives agentkit's Shaper over recorded dun sessions, offline.
//
// Load reads a recorded dun.jsonl event stream into the message history a
// Shaper Store serves. Run drives Shaper.Build round by round with a fake
// zero-network LLMRunner and reports prompt size, prefix kept, shrink events,
// and cost at ledger prices per round. No model calls are made.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	agent "github.com/iodesystems/agentkit/agent"
)

// RoundMeta carries the provider's token counts from one round's request event
// and the number of entries in the store when that round's Build was called.
// EntryCount is the exclusive upper bound: entries[0:EntryCount] were in the
// store. The provider counts are the ground-truth figures the provider reported
// for that request; the driver uses them for cost, not for prompt size.
type RoundMeta struct {
	// EntryCount is how many entries the Shaper saw for this round's Build call.
	EntryCount int
	// Provider token counts from the request event (phase "done").
	PromptTokens int
	CachedTokens int
	GenTokens    int
}

// userPayload is the "user" event: the user's input for a turn.
type userPayload struct {
	ID      string `json:"id,omitempty"`
	Content string `json:"content"`
}

// messagePayload is the "message" event: the assistant's final reply for a round.
type messagePayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	ID      string `json:"id,omitempty"`
}

// toolCallPayload is the "tool_call" event: a tool call the model requested.
type toolCallPayload struct {
	CallID string         `json:"call_id,omitempty"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
}

// toolResultPayload is the "tool_result" event: the result of a tool call.
type toolResultPayload struct {
	CallID  string `json:"call_id,omitempty"`
	Tool    string `json:"tool"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
}

// compactionPayload is the "compaction" event: a fold the engine performed.
type compactionPayload struct {
	Text         string `json:"text"`
	Subsumed     int    `json:"subsumed"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
}

// requestPayload is the "request" event with phase "done": the provider's
// token counts for one round.
type requestPayload struct {
	Phase  string `json:"phase"`
	Prompt int    `json:"prompt,omitempty"`
	Cached int    `json:"cached,omitempty"`
	Gen    int    `json:"gen,omitempty"`
}

// eventEnvelope is the JSON envelope every dun.jsonl line carries.
type eventEnvelope struct {
	Type string `json:"type"`
}

// Load reads a recorded dun.jsonl event stream and returns the reconstructed
// message history (as agent.Entry values) and one RoundMeta per model round.
//
// The event stream is the engine's JSONL log: one object per line, each with a
// "type" field. Load maps user, message, tool_call, tool_result, and compaction
// events onto agent.Entry values in order, and records a RoundMeta at each
// "request" event with phase "done" (the round boundary, carrying the provider's
// token counts).
//
// Token events (streamed fragments) and other event types are ignored: they
// carry no history. The CreatedAt field is assigned sequentially (nanoseconds,
// starting at 1) so entries are in file order, which is the order the engine
// appended them.
//
// There are no blob references in the event stream: tool results are capped
// inline by the engine before they are written to the log.
func Load(path string) ([]agent.Entry, []RoundMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("replay: open %s: %w", path, err)
	}
	defer f.Close()

	var (
		entries []agent.Entry
		rounds  []RoundMeta
		seq     int64 = 1
	)

	scanner := bufio.NewScanner(f)
	// Tool results can be large (up to 64 KiB in the engine); give headroom
	// for the JSON envelope.
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var env eventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return nil, nil, fmt.Errorf("replay: line %d: parse envelope: %w", lineNum, err)
		}

		switch env.Type {
		case "user":
			var p userPayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse user: %w", lineNum, err)
			}
			entries = append(entries, agent.Entry{
				ID:        p.ID,
				Kind:      agent.KindUser,
				Content:   p.Content,
				CreatedAt: seq,
			})
			seq++

		case "message":
			var p messagePayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse message: %w", lineNum, err)
			}
			// Only assistant messages go into the history; the user's input
			// is already recorded as a "user" event.
			if p.Role != "assistant" {
				continue
			}
			entries = append(entries, agent.Entry{
				ID:        p.ID,
				Kind:      agent.KindAssistant,
				Content:   p.Content,
				CreatedAt: seq,
			})
			seq++

		case "tool_call":
			var p toolCallPayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse tool_call: %w", lineNum, err)
			}
			argsJSON, _ := json.Marshal(p.Args)
			entries = append(entries, agent.Entry{
				Kind:       agent.KindToolCall,
				ToolName:   p.Tool,
				Content:    string(argsJSON),
				ToolCallID: p.CallID,
				CreatedAt:  seq,
			})
			seq++

		case "tool_result":
			var p toolResultPayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse tool_result: %w", lineNum, err)
			}
			entries = append(entries, agent.Entry{
				Kind:       agent.KindToolResult,
				ToolName:   p.Tool,
				Content:    p.Result,
				ToolCallID: p.CallID,
				CreatedAt:  seq,
			})
			seq++

		case "compaction":
			var p compactionPayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse compaction: %w", lineNum, err)
			}
			entries = append(entries, agent.Entry{
				Kind:      agent.KindCompaction,
				Content:   p.Text,
				CreatedAt: seq,
			})
			seq++

		case "request":
			var p requestPayload
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				return nil, nil, fmt.Errorf("replay: line %d: parse request: %w", lineNum, err)
			}
			// Only phase "done" marks a round boundary with provider counts.
			if p.Phase != "done" {
				continue
			}
			rounds = append(rounds, RoundMeta{
				EntryCount:   len(entries),
				PromptTokens: p.Prompt,
				CachedTokens: p.Cached,
				GenTokens:    p.Gen,
			})

		default:
			// token, done, usage, session, and all other event types are not
			// part of the message history.
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("replay: read %s: %w", path, err)
	}

	if len(rounds) == 0 {
		return nil, nil, fmt.Errorf("replay: %s: no request events with phase \"done\" found", path)
	}

	return entries, rounds, nil
}
