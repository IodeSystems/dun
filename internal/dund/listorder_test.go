package dund

import (
	"os"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

func TestSortSessionsRunningThenActivityThenID(t *testing.T) {
	items := []proto.SessionSummary{
		{ID: "a", LastActive: "2026-01-03T00:00:00Z"},
		{ID: "b", Running: true, LastActive: "2026-01-01T00:00:00Z"},
		{ID: "c", LastActive: "2026-01-03T00:00:00Z"},
		{ID: "d"},
		{ID: "e", Running: true, LastActive: "2026-01-02T00:00:00Z"},
	}
	proto.SortSessions(items)
	got := ""
	for _, s := range items {
		got += s.ID
	}
	if got != "ebcad" {
		t.Fatalf("order %q, want ebcad", got)
	}
}

// List adds on-disk transcripts the registry lacks, newest N, with no
// sidecar files and no empty ones, ordered by last activity.
func TestListIncludesTranscriptsOnDisk(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	svc, _, _ := newSvc(t)
	ws := t.TempDir()
	id, err := svc.Create(alice, ws, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := dun.RootDir(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, size int, age time.Duration) {
		p := dir + "/" + name
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		_ = os.Chtimes(p, mt, mt)
	}
	write("19990101-000000.jsonl", 5, 48*time.Hour)
	write("19990102-000000.jsonl", 5, 24*time.Hour)
	write("19990102-000000.plan.jsonl", 5, 0) // sidecar
	write("19990103-000000.jsonl", 0, 0)      // empty
	for i := 0; i < diskPerWorkspace+5; i++ {
		write("1980"+string(rune('a'+i%26))+string(rune('a'+i/26))+".jsonl", 5, 72*time.Hour)
	}

	items := svc.List(alice)
	var ids []string
	for _, s := range items {
		ids = append(ids, s.ID)
		if s.ID != id && (s.Running || s.Workspace != ws || s.LastActive == "") {
			t.Errorf("on-disk row %+v: want running=false, workspace, lastActive", s)
		}
	}
	if len(items) != 1+diskPerWorkspace {
		t.Fatalf("got %d rows, want %d: %v", len(items), 1+diskPerWorkspace, ids)
	}
	pos := map[string]int{}
	for i, x := range ids {
		pos[x] = i
	}
	for _, bad := range []string{"19990102-000000.plan", "19990103-000000"} {
		if _, ok := pos[bad]; ok {
			t.Errorf("%s listed", bad)
		}
	}
	// Ids ascend with age here, so the newest 20 ids hold 19990102 and 19990101.
	if _, ok := pos["19990102-000000"]; !ok {
		t.Fatalf("19990102 missing: %v", ids)
	}
	if pos["19990102-000000"] > pos["19990101-000000"] {
		t.Errorf("more recently active should come first: %v", ids)
	}
	// Another principal sees none of it.
	if got := svc.List(bob); len(got) != 0 {
		t.Errorf("bob sees %d rows", len(got))
	}
}
