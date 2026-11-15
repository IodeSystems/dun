package dund

import (
	"testing"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// A reconnect by snapshot, twice: every state event in it is marked
// replayed (a client shows no note for it), both times.
func TestSnapshotStateIsMarkedReplayed(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), []string{"--model", "m"}, "")
	ses, _ := svc.Get(alice, id)
	ses.log.Append(proto.NewEvent(&proto.ServerEvent{ID: "rag", Action: "on", Message: "rag: on"}))
	for range 2 {
		snap, _, sub, err := ses.subscribe(1, "an old epoch")
		if err != nil || snap == nil {
			t.Fatal(err)
		}
		sub.Close()
		st := snap.Payload.(*proto.Snapshot).State
		if len(st) == 0 {
			t.Fatal("no state")
		}
		for _, ev := range st {
			if !ev.Replayed {
				t.Errorf("%s not marked replayed", ev.Type)
			}
		}
	}
	// The log's own copy is not marked: a live client sees it as news.
	for _, ev := range ses.log.State() {
		if ev.Replayed {
			t.Errorf("the log's %s was marked", ev.Type)
		}
	}
}

// What a snapshot's items hold is not replayed after it again.
func TestAfterSnapshotDropsWhatTheItemsHold(t *testing.T) {
	items := []dun.HistoryItem{
		{Kind: "user", ID: "u1", Content: "hi"},
		{Kind: "tool_call", CallID: "c1", Tool: "exec"},
		{Kind: "tool_result", CallID: "c1", Tool: "exec"},
		{Kind: "assistant", Content: "done."},
	}
	replay := []proto.Event{
		proto.NewEvent(&proto.UserEcho{ID: "u1", Content: "hi"}),
		proto.NewEvent(&proto.ToolCall{CallID: "c1", Tool: "exec"}),
		proto.NewEvent(&proto.ToolResult{CallID: "c1", Tool: "exec"}),
		proto.NewEvent(&proto.Message{Role: "assistant", Content: "done."}),
		proto.NewEvent(&proto.UserEcho{ID: "u2", Content: "next"}),
		proto.NewEvent(&proto.ToolCall{CallID: "c2", Tool: "exec"}),
		proto.NewEvent(&proto.UserEcho{Content: "an old engine's, no id"}),
	}
	got := afterSnapshot(items, replay)
	if len(got) != 3 || got[0].Payload.(*proto.UserEcho).ID != "u2" || got[1].Payload.(*proto.ToolCall).CallID != "c2" {
		var types []string
		for _, ev := range got {
			types = append(types, ev.Type)
		}
		t.Fatalf("kept %v", types)
	}
}

// Two identical replies in a row both survive a reconnect: the one the
// items hold is dropped by its id, the next one (same text, new id) is not.
// An old engine's reply (no id) falls back to the text rule.
func TestAfterSnapshotDedupesRepliesByID(t *testing.T) {
	items := []dun.HistoryItem{
		{Kind: "user", ID: "u1", Content: "ping"},
		{Kind: "assistant", ID: "r1", Content: "pong"},
	}
	replay := []proto.Event{
		proto.NewEvent(&proto.Message{Role: "assistant", Content: "pong", ID: "r1"}),
		proto.NewEvent(&proto.UserEcho{ID: "u2", Content: "ping"}),
		proto.NewEvent(&proto.Message{Role: "assistant", Content: "pong", ID: "r2"}),
	}
	got := afterSnapshot(items, replay)
	if len(got) != 2 || got[1].Payload.(*proto.Message).ID != "r2" {
		t.Fatalf("kept %d: %+v", len(got), got)
	}
	old := afterSnapshot(items, []proto.Event{proto.NewEvent(&proto.Message{Role: "assistant", Content: "pong"})})
	if len(old) != 0 {
		t.Errorf("an old engine's repeat of the last reply was kept")
	}
}
