package dund

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// A remote MCP server with one tool, counting handshakes.
func gwUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var inits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			inits.Add(1)
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "a", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(ts.Close)
	return ts, &inits
}

// mcp_share over two engines' stdio: one upstream, an endpoint and a
// bearer each; an engine's exit releases its client.
func TestMcpShare_TwoEnginesOneUpstream(t *testing.T) {
	svc, sp, _ := newSvc(t)
	ts, inits := gwUpstream(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	req, _ := json.Marshal(dun.ShareRequest{Spec: dun.ShareSpec{ID: "el", URL: ts.URL}, Instance: dun.InstanceShared})
	share := func(id, rid string) proto.McpShareRep {
		t.Helper()
		js, _ := json.Marshal(proto.PeerRequest{RID: rid, Op: "mcp_share", Share: &proto.McpShareReq{Request: string(req)}})
		if err := svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "peer-request " + string(js)})); err != nil {
			t.Fatal(err)
		}
		r := waitRecord(t, sp, "reply "+id+`: {"rid":"`+rid+`"`)
		var rep proto.PeerReply
		if err := json.Unmarshal([]byte(strings.TrimPrefix(r, "reply "+id+": ")), &rep); err != nil || rep.Error != "" || rep.Share == nil {
			t.Fatalf("share reply %s: %v", r, err)
		}
		return *rep.Share
	}
	g1 := share(a1, "r1")
	g2 := share(a2, "r2")
	if g1.URL != g2.URL || g1.Key != g2.Key || g1.Bearer == g2.Bearer || !strings.HasPrefix(g1.URL, "http://127.0.0.1:") {
		t.Fatalf("grants: %+v %+v", g1, g2)
	}
	gw := svc.gateway()
	if gw.Starts() != 1 || inits.Load() != 1 || gw.Clients(g1.Key) != 2 {
		t.Fatalf("starts %d, handshakes %d, clients %d", gw.Starts(), inits.Load(), gw.Clients(g1.Key))
	}
	// The endpoint answers its bearer, and no other.
	post := func(bearer string) int {
		r, _ := http.NewRequest(http.MethodPost, g1.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post(g1.Bearer) != http.StatusOK || post("nope") != http.StatusNotFound {
		t.Error("the endpoint's bearer check")
	}
	// a1's engine exits: its client is released, a2's stays.
	_ = svc.Send(alice, a1, proto.NewCommand("", &proto.User{Content: "crash"}))
	deadline := time.Now().Add(3 * time.Second)
	for gw.Clients(g1.Key) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gw.Clients(g1.Key) != 1 || post(g1.Bearer) != http.StatusNotFound || post(g2.Bearer) != http.StatusOK {
		t.Errorf("after a1's exit: %d clients", gw.Clients(g1.Key))
	}
	// mcp_release gives a client back.
	js, _ := json.Marshal(proto.PeerRequest{RID: "r3", Op: "mcp_release", Share: &proto.McpShareReq{Release: g2.Bearer}})
	_ = svc.Send(alice, a2, proto.NewCommand("", &proto.User{Content: "peer-request " + string(js)}))
	waitRecord(t, sp, "reply "+a2+`: {"rid":"r3"`)
	if gw.Clients(g1.Key) != 0 {
		t.Errorf("after the release: %d clients", gw.Clients(g1.Key))
	}
}
