package acp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
)

// Steering (docs/acp.md, "Steering"): JetBrains Air sends a message typed
// while a turn runs as `_session/steering`, an extension request, not as a
// second session/prompt. dun takes it as it takes a prompt that comes
// mid-turn (turns.go): a user message dun queues and delivers into the
// turn's next tool result. Nothing waits on it: the prompt that started
// the turn is answered at the turn's end, as before.
//
// The params' shape is not published. An Air debug bundle (2026-11-24,
// keys pseudonymised) shows {sessionId, <a string>, <an array of objects of
// two strings>}: read here as an id and the message's content blocks
// ({type, text}). So the text is taken from the first array that reads as
// content blocks; else from a string at a key such as text or message;
// else from the one string there is. The response in the bundle is an
// object with one string member; its key is unknown, so the bridge answers
// {"status": "queued"}.

const methodSteering = "_session/steering"

// textKeys are the keys a steering message's text may be under.
var textKeys = []string{"text", "message", "content", "prompt", "steering", "input"}

var _ sdk.ExtensionMethodHandler = (*Agent)(nil)

// HandleExtensionMethod answers the extension requests dun knows: only
// _session/steering.
func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != methodSteering {
		return nil, sdk.NewMethodNotFound(method)
	}
	var p struct {
		SessionId sdk.SessionId `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionId == "" {
		return nil, sdk.NewInvalidParams(map[string]any{"error": "sessionId is required"})
	}
	s, err := a.session(p.SessionId)
	if err != nil {
		return nil, err
	}
	text := steeringText(params)
	if text == "" {
		return nil, sdk.NewInvalidParams(map[string]any{"error": "the steering message has no text"})
	}
	if _, err := s.submit(text); err != nil {
		return nil, err
	}
	return map[string]string{"status": "queued"}, nil
}

// steeringText finds the message's text in the params (see above); "" when
// there is none.
func steeringText(params json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(params, &m) != nil {
		return ""
	}
	delete(m, "sessionId")
	if t := blocksIn(m); t != "" {
		return t
	}
	if t := keyedIn(m); t != "" {
		return t
	}
	var all []string
	strs(m, &all)
	if len(all) == 1 {
		return strings.TrimSpace(all[0])
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// blocksIn is the text of the first array, at any depth, that reads as
// content blocks.
func blocksIn(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if t := blocksIn(x[k]); t != "" {
				return t
			}
		}
	case []any:
		if b, err := json.Marshal(x); err == nil {
			var blocks []sdk.ContentBlock
			if json.Unmarshal(b, &blocks) == nil {
				if t := promptText(blocks); t != "" {
					return t
				}
			}
		}
		for _, e := range x {
			if t := blocksIn(e); t != "" {
				return t
			}
		}
	}
	return ""
}

// keyedIn is the first non-empty string, at any depth, under one of
// textKeys.
func keyedIn(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if s, ok := x[k].(string); ok && slices.Contains(textKeys, strings.ToLower(k)) && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, k := range sortedKeys(x) {
			if t := keyedIn(x[k]); t != "" {
				return t
			}
		}
	case []any:
		for _, e := range x {
			if t := keyedIn(e); t != "" {
				return t
			}
		}
	}
	return ""
}

// strs collects the non-empty strings at any depth.
func strs(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) != "" {
			*out = append(*out, x)
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			strs(x[k], out)
		}
	case []any:
		for _, e := range x {
			strs(e, out)
		}
	}
}
