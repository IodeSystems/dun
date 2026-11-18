package dund

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// dund's procs answer carries retrieval when it runs: each open index with
// its counters, and each session's project slug to match them to. Without
// retrieval there is no rag section.
func TestProcs_CarriesRag(t *testing.T) {
	svc, _, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := NewServer(svc, UnixPeer)
	go srv.Serve(ln)
	a := dial(t, sock)
	procs := func(id string) *proto.ProcsList {
		a.send(id, "", &proto.Procs{})
		ev := a.until(func(ev proto.Event) bool { return ev.Re == id })
		l, ok := ev.Payload.(*proto.ProcsList)
		if !ok {
			t.Fatalf("procs answered %s %+v", ev.Type, ev.Payload)
		}
		return l
	}
	if l := procs("p0"); l.Rag != nil {
		t.Fatalf("rag with no retrieval set: %+v", l.Rag)
	}

	h, err := ragd.Open(t.TempDir(), ragd.Roles{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(time.Second)
	srv.SetRag(h)
	doc := filepath.Join(t.TempDir(), "n.md")
	if err := os.WriteFile(doc, []byte("narwhal tusk survey\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.send("i1", "", &proto.RagIngest{Project: "dun", Index: "docs", Path: doc})
	a.until(func(ev proto.Event) bool { return ev.Re == "i1" })

	l := procs("p1")
	if l.Rag == nil {
		t.Fatal("no rag section with retrieval set")
	}
	var docs *proto.ProcRagIndex
	for i, ix := range l.Rag.Indexes {
		if ix.Name == "dun/docs" {
			docs = &l.Rag.Indexes[i]
		}
	}
	if docs == nil || docs.Project != "dun" || docs.Kind != "docs" || docs.State == "" {
		t.Fatalf("dun/docs: %+v (all: %+v)", docs, l.Rag.Indexes)
	}
	if l.Rag.Embed.Model != "" {
		t.Errorf("embed model with none set: %+v", l.Rag.Embed)
	}
}

// A session's procs entry names its workspace's project slug.
func TestProcs_SessionProject(t *testing.T) {
	svc, _, _ := newSvc(t)
	ws := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(alice, ws, nil, ""); err != nil {
		t.Fatal(err)
	}
	l := svc.Procs(alice)
	if len(l.Sessions) != 1 {
		t.Fatalf("procs: %+v", l)
	}
	if want := harness.ProjectSlug(ws); l.Sessions[0].Project != want || want == "" {
		t.Fatalf("project %q, want %q", l.Sessions[0].Project, want)
	}
}
