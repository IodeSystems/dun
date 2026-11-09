package dun_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// dundRag is a dund serving retrieval (FTS only) on a socket; down and up
// take its listener away and back.
type dundRag struct {
	t    *testing.T
	sock string
	srv  *dund.Server
	ln   net.Listener
	host *ragd.Host
}

func serveDund(t *testing.T) *dundRag {
	t.Helper()
	svc, err := dund.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	host, err := ragd.Open(t.TempDir(), ragd.Roles{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(time.Second) })
	d := &dundRag{t: t, sock: filepath.Join(t.TempDir(), "d.sock"), srv: dund.NewServer(svc, dund.UnixPeer), host: host}
	d.srv.SetRag(host)
	d.up()
	t.Cleanup(d.down)
	return d
}

func (d *dundRag) up() {
	d.t.Helper()
	ln, err := net.Listen("unix", d.sock)
	if err != nil {
		d.t.Fatal(err)
	}
	d.ln = ln
	go d.srv.Serve(ln)
}

func (d *dundRag) down() {
	if d.ln != nil {
		d.ln.Close()
		d.ln = nil
	}
	os.Remove(d.sock)
}

// find polls an index until a search for query holds want (a path
// fragment), or fails.
func (d *dundRag) find(project, query, want string, tags ...string) {
	d.t.Helper()
	d.findIn("memories", project, query, want, tags...)
}

func (d *dundRag) findIn(index, project, query, want string, tags ...string) {
	d.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := d.host.Search(context.Background(), proto.RagSearch{Project: project, Index: index, Query: query, Mode: "fts",
			Filter: proto.RagFilter{Tags: tags}})
		if err != nil {
			d.t.Fatal(err)
		}
		for _, h := range r.Hits {
			if strings.Contains(h.Path, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("%q in %q (tags %v): no %s after 30s: %+v", query, project, tags, want, r.Hits)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *dundRag) none(project, query string, tags ...string) {
	d.t.Helper()
	r, err := d.host.Search(context.Background(), proto.RagSearch{Project: project, Index: "memories", Query: query, Mode: "fts",
		Filter: proto.RagFilter{Tags: tags}})
	if err != nil {
		d.t.Fatal(err)
	}
	if len(r.Hits) != 0 {
		d.t.Fatalf("%q in %q (tags %v): want no hits, got %+v", query, project, tags, r.Hits)
	}
}

func memoryCall(t *testing.T, h *dun.Harness, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "memory", string(b)
	out, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// End to end over dund's socket (docs/rag.md, slice 5): a note the memory
// tool writes is found in dund's memories index of its scope, its tags
// filter, and a note written while dund was down is indexed on the next
// contact.
func TestMemoryNotesGoToDund(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	d := serveDund(t)
	ws := filepath.Join(t.TempDir(), "harbor")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := dun.Start(context.Background(), dun.Config{Workspace: ws, Rag: dun.DundRag(d.sock)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	out := memoryCall(t, h, map[string]any{"action": "add", "question": "how do deploys reach the cluster?",
		"content": "Deploys go through the zebra cluster.", "tags": []string{"Deploy"}})
	if !strings.HasPrefix(out, "saved ") || strings.Contains(out, "not reachable") {
		t.Fatalf("project add: %q", out)
	}
	out = memoryCall(t, h, map[string]any{"action": "add", "scope": "global", "content": "The user wants zebra-sized fragments, not prose."})
	if !strings.HasSuffix(out, "(global)") {
		t.Fatalf("global add: %q", out)
	}
	d.find("harbor", "zebra", "how-do-deploys")
	d.find("", "zebra", "@global")
	// Scope: each index holds its own notes only.
	d.find("harbor", "zebra", "how-do-deploys", "deploy")
	d.none("harbor", "fragments")
	d.none("", "cluster")
	// Tags: whole-tag AND.
	d.none("harbor", "zebra", "deploy", "absent")

	// dund down: the note is kept, and said not indexed.
	d.down()
	out = memoryCall(t, h, map[string]any{"action": "add", "content": "The narwhal job needs a warm cache."})
	if !strings.Contains(out, "dund is not reachable") {
		t.Fatalf("add with dund down: %q", out)
	}
	out = memoryCall(t, h, map[string]any{"action": "void", "old_text": "zebra cluster", "why": "moved to yak", "source": "user"})
	if !strings.Contains(out, "voided") || !strings.Contains(out, "not reachable") {
		t.Fatalf("void with dund down: %q", out)
	}
	// Back: the first call that reaches dund syncs the notes.
	d.up()
	memoryCall(t, h, map[string]any{"action": "add", "content": "The otter build is slow on Mondays."})
	d.find("harbor", "narwhal", "narwhal")
	d.find("harbor", "otter", "otter")
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, _ := d.host.Search(context.Background(), proto.RagSearch{Project: "harbor", Index: "memories", Query: "zebra", Mode: "fts"})
		if len(r.Hits) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the note voided while dund was down is still indexed: %+v", r.Hits)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Strategies and constraints over dund's socket: they go to the
// strategies index of their scope (not memories), and file_read reads one
// by its path.
func TestStrategiesGoToDund(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	d := serveDund(t)
	ws := filepath.Join(t.TempDir(), "harbor")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := dun.Start(context.Background(), dun.Config{Workspace: ws, Rag: dun.DundRag(d.sock)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	out := memoryCall(t, h, map[string]any{"action": "add", "kind": "strategy", "name": "bisect quokka",
		"content": "Bisect the quokka failure with -count=20 first."})
	if !strings.Contains(out, "strategy:bisect-quokka") || strings.Contains(out, "not reachable") {
		t.Fatalf("strategy add: %q", out)
	}
	if _, err := h.MemoriesCommand("add constraint global no-wombat Never touch the wombat config."); err != nil {
		t.Fatal(err)
	}
	d.findIn("strategies", "harbor", "quokka", "strategy-bisect-quokka")
	d.findIn("strategies", "", "wombat", "constraint-no-wombat")
	d.none("harbor", "quokka")
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "file_read", `{"path":"constraint:no-wombat"}`
	if out, _ := h.Session.Dispatch(context.Background(), tc); !strings.Contains(out, "wombat config") {
		t.Fatalf("file_read constraint: %q", out)
	}
}
