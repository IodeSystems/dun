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

// The colour request is asked back with ask_user and its arguments, then
// answered once the result is in.
func TestCannedReply_ColourAsksFirst(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"ask_user"}}`)}
	req := chatRequest{Tools: tools, Messages: []json.RawMessage{msg("user", "Change the greeting colour")}}
	if reply, call := cannedReply(req); call != "ask_user" || reply != "" {
		t.Fatalf("first answer: reply %q call %q", reply, call)
	}
	var a struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal([]byte(askArgs), &a); err != nil || a.Question == "" || len(a.Options) < 2 {
		t.Fatalf("askArgs: %v %+v", err, a)
	}
	req.Messages = append(req.Messages, msg("assistant", ""), msg("tool", "blue"))
	if reply, call := cannedReply(req); call != "" || reply == "" {
		t.Fatalf("after the answer: reply %q call %q", reply, call)
	}
}
