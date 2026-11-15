package dundapi_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitEvent polls the session's events until one has the type and passes ok.
func waitEvent(t *testing.T, srv *httptest.Server, tok, id, typ string, ok func(map[string]any) bool) {
	t.Helper()
	var after float64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, m := call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/events?after="+jsonNum(after)+"&wait=2", nil)
		evs, _ := m["events"].([]any)
		for _, e := range evs {
			if ev := e.(map[string]any); ev["type"] == typ && ok(ev) {
				return
			}
		}
		after, _ = m["last"].(float64)
	}
	t.Fatalf("no %s event", typ)
}

// An agent view goes to the engine, whose history answers in the events.
func TestAgentOverHTTP(t *testing.T) {
	srv, tok, _ := setup(t)
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	id := m["id"].(string)
	if code, m := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/agent", map[string]any{"agent": "2", "action": "view"}); code != 200 {
		t.Fatalf("view: %d %v", code, m)
	}
	waitEvent(t, srv, tok, id, "agent_history", func(ev map[string]any) bool { return ev["agent"] == float64(2) })
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/agent", map[string]any{"agent": "2", "action": "rm"}); code != 422 {
		t.Errorf("an unknown action: %d", code)
	}
}

// A job's output is the log its jobs event named, by job id only.
func TestJobOutputOverHTTP(t *testing.T) {
	srv, tok, _ := setup(t)
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	id := m["id"].(string)
	log := filepath.Join(t.TempDir(), "job-1.log")
	if err := os.WriteFile(log, []byte("ok  pkg 0.1s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "job " + log})
	waitEvent(t, srv, tok, id, "jobs", func(map[string]any) bool { return true })
	code, m := call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/jobs/1/output?tail=5", nil)
	if code != 200 || m["data"] != "0.1s\n" || m["next"] != float64(13) || m["state"] != "exited" {
		t.Fatalf("tail: %d %v", code, m)
	}
	if code, m = call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/jobs/1/output", nil); m["data"] != "ok  pkg 0.1s\n" {
		t.Fatalf("whole: %d %v", code, m)
	}
	if code, _ = call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/jobs/7/output", nil); code != 400 {
		t.Errorf("an unknown job: %d", code)
	}
	if code, _ = call(t, srv, tok, "GET", "/api/v1/sessions/nope/jobs/1/output", nil); code != 404 {
		t.Errorf("an unknown session: %d", code)
	}
}
