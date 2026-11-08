package dund

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// The rag commands go to dund itself: refused when retrieval is not set,
// answered with their reply type when it is.
func TestRagCommandsOverTheSocket(t *testing.T) {
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
	a.send("r0", "", &proto.RagJobs{Index: "memories"})
	if p := a.until(ackOf("r0")).Payload.(*proto.Ack); p.Error == "" {
		t.Fatal("rag_jobs answered with no retrieval set")
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
	a.send("r1", "", &proto.RagIngest{Project: "dun", Index: "docs", Path: doc})
	ing := a.until(func(ev proto.Event) bool { return ev.Re == "r1" })
	if _, ok := ing.Payload.(*proto.RagIngested); !ok {
		t.Fatalf("ingest answered %s %+v", ing.Type, ing.Payload)
	}
	a.send("r1b", "", &proto.RagReembed{Project: "dun", Index: "docs"})
	if p, ok := a.until(func(ev proto.Event) bool { return ev.Re == "r1b" }).Payload.(*proto.Ack); !ok || !strings.Contains(p.Error, "no embed model") {
		t.Fatalf("rag_reembed with no embed model: %+v", p)
	}
	a.send("r2", "", &proto.RagSearch{Project: "dun", Index: "bogus", Query: "narwhal"})
	if p, ok := a.until(func(ev proto.Event) bool { return ev.Re == "r2" }).Payload.(*proto.Ack); !ok || p.Error == "" {
		t.Fatal("an unknown index was not refused")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		a.send("r3", "", &proto.RagSearch{Project: "dun", Index: "docs", Query: "narwhal"})
		ev := a.until(func(ev proto.Event) bool { return ev.Re == "r3" })
		hits, ok := ev.Payload.(*proto.RagHits)
		if !ok {
			t.Fatalf("search answered %s %+v", ev.Type, ev.Payload)
		}
		if len(hits.Hits) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no hits after 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
