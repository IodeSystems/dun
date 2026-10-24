package main

import (
	"encoding/json"
	"testing"
)

func msg(role, content string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"role": role, "content": content})
	return b
}

// The notes question is answered with a call to the notes tool first, then
// from its result — the order a real model takes once the server is added.
func TestCannedReply_NotesCallsTheToolFirst(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"notes_project_notes"}}`)}
	req := chatRequest{Tools: tools, Messages: []json.RawMessage{msg("user", "What do the team notes say?")}}
	if reply, call := cannedReply(req); call != "notes_project_notes" || reply != "" {
		t.Fatalf("first answer: reply %q call %q", reply, call)
	}
	req.Messages = append(req.Messages, msg("assistant", ""), msg("tool", "Notes: keep the greeting short."))
	if reply, call := cannedReply(req); call != "" || reply == "" {
		t.Fatalf("after the result: reply %q call %q", reply, call)
	}
	// Without the server, no call: a plain answer.
	req = chatRequest{Tools: []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"eval"}}`)},
		Messages: []json.RawMessage{msg("user", "What do the team notes say?")}}
	if _, call := cannedReply(req); call != "" {
		t.Fatalf("called %q with no notes tool", call)
	}
}
