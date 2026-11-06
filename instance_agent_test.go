package dun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// countStarts is how many times the fake server was started.
func countStarts(log string) int {
	n := 0
	for _, l := range fakeLog(log) {
		if strings.HasPrefix(l, "start ") {
			n++
		}
	}
	return n
}

// instance agent: a sub-agent gets its own connection under the same id,
// so the model's tool names do not change, and it stops with the child.
// Without dund it is the child's own start; the session's is its own, as
// session is.
func TestInstanceAgent_ChildHasItsOwn(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, inits := countingMCP(t)
	ctx := context.Background()
	h, err := Start(ctx, Config{Workspace: t.TempDir(), Client: &scriptRunner{},
		Servers: []Server{{ID: "ctr", URL: ts.URL, Autostart: true, Instance: InstanceAgent}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.WaitServers(ctx)
	if inits.Load() != 1 {
		t.Fatalf("the session's: %d handshakes", inits.Load())
	}
	sa, err := h.spawnAgent(ctx, "look", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	child := sa.h
	if !child.own["ctr"] || !child.ownMgr.ServerStarted("ctr") || inits.Load() != 2 {
		t.Fatalf("the child's own: own %v, started %v, %d handshakes", child.own, child.ownMgr.ServerStarted("ctr"), inits.Load())
	}
	if !slices.Contains(modelTools(child), "ctr__echo") {
		t.Errorf("the child's tool names: %v", modelTools(child))
	}
	if out, err := (childCaller{child}).CallTool(ctx, "ctr", "echo", map[string]any{"x": "c"}); err != nil || out != "echo c" {
		t.Errorf("the child's call: %q %v", out, err)
	}
	own := child.ownMgr
	sa.quit()
	if own.ServerStarted("ctr") {
		t.Error("the child's own instance outlived it")
	}
	if !h.mgr.ServerStarted("ctr") {
		t.Error("the child's quit stopped the session's")
	}
}

// Under dund, a child's agent instance is a gateway key of its own (the
// session and the agent's path); the child's close releases it.
func TestInstanceAgent_ChildThroughTheGateway(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, inits := countingMCP(t)
	g := newGateway(t)
	ctx := context.Background()
	h, err := Start(ctx, Config{Workspace: t.TempDir(), Client: &scriptRunner{},
		Servers: []Server{{ID: "ctr", URL: ts.URL, Autostart: true, Instance: InstanceAgent}},
		Gateway: gwLink{g: g, c: GatewayClient{Session: "s1", Engine: "e1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.WaitServers(ctx)
	if g.Starts() != 0 || inits.Load() != 1 {
		t.Fatalf("the session's agent instance is its own: %d gateway starts", g.Starts())
	}
	sa, err := h.spawnAgent(ctx, "look", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	grant := sa.h.shared["ctr"]
	if g.Starts() != 1 || grant.Key == "" || g.Clients(grant.Key) != 1 {
		t.Fatalf("the child's: %d starts, grant %+v", g.Starts(), grant)
	}
	if want := (ShareRequest{Spec: ShareSpec{ID: "ctr", URL: ts.URL, Timeout: 90}, Instance: InstanceAgent, Agent: "1"}).Key("s1"); grant.Key != want {
		t.Errorf("key %s, want the session's and agent 1's %s", grant.Key, want)
	}
	sa.quit()
	if g.Clients(grant.Key) != 0 {
		t.Errorf("the child's close left %d clients", g.Clients(grant.Key))
	}
}

// A child isolated in a worktree gets its own instance of each server keyed
// on {{workspace}}, rooted in its tree; a server not keyed on it stays the
// parent's.
func TestIsolatedChild_OwnInstanceForItsTree(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "base")
	ts, inits := countingMCP(t)
	cmd, log := fakeBin(t, "tree-mcp", "poly-lsp")
	g := newGateway(t)
	ctx := context.Background()
	h, err := Start(ctx, Config{Workspace: repo, Client: &scriptRunner{}, Exec: &HostShell{Dir: repo},
		Servers: []Server{
			{ID: "tree", Command: cmd, Args: []string{"mcp", "--root", "{{workspace}}"}, Autostart: true},
			{ID: "ctr", URL: ts.URL, Autostart: true},
		},
		Gateway: gwLink{g: g, c: GatewayClient{Session: "s1", Engine: "e1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.WaitServers(ctx)
	if g.Starts() != 2 || countStarts(log) != 1 {
		t.Fatalf("the session's: %d gateway starts, %d tree starts", g.Starts(), countStarts(log))
	}
	plain, err := h.spawnAgent(ctx, "look", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, plain)
	if len(plain.h.own) != 0 || g.Starts() != 2 {
		t.Errorf("a child in its parent's tree has its own %v (%d starts)", plain.h.own, g.Starts())
	}
	sa, err := h.spawnAgent(ctx, "edit", "", "", "", IsoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	child := sa.h
	if !child.own["tree"] || child.own["ctr"] {
		t.Fatalf("own: %v", child.own)
	}
	if g.Starts() != 3 || countStarts(log) != 2 || inits.Load() != 1 {
		t.Errorf("after the isolated child: %d gateway starts, %d tree starts, %d ctr handshakes", g.Starts(), countStarts(log), inits.Load())
	}
	spec, _ := child.knownServer("tree")
	if !slices.Contains(spec.Args, sa.iso.Path) {
		t.Errorf("the child's tree server: %v, want its worktree %s", spec.Args, sa.iso.Path)
	}
	if child.shared["tree"].Key == h.shared["tree"].Key {
		t.Error("the child's tree server shares the parent's key")
	}
}

// poly-lsp is keyed on the workspace, raglit is not.
func TestWorkspaceKeyed_Builtins(t *testing.T) {
	if !workspaceKeyed(ServerSpec{ID: ServerCode}) || workspaceKeyed(ServerSpec{ID: ServerDocs}) || workspaceKeyed(ServerSpec{ID: ServerShell}) {
		t.Error("poly-lsp keyed, raglit and the shell not")
	}
	if !workspaceKeyed(ServerSpec{ID: "x", Env: []string{"ROOT={{workspace}}"}}) {
		t.Error("an env naming the workspace")
	}
}
