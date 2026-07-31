package dun

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// Describing a tool family whose tools are absent is worse than saying nothing:
// the model plans around node_query, calls it, and gets "unknown tool".
func TestSystemFor_OnlyDescribesRunningFamilies(t *testing.T) {
	shellOnly := systemFor([]mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}, HostExec{Dir: "/tmp"}, nil)
	if strings.Contains(shellOnly, "node_query") {
		t.Error("described code tools with no code server running")
	}
	if strings.Contains(shellOnly, "raglit") {
		t.Error("described docs tools with no docs server running")
	}
	if !strings.Contains(shellOnly, "mcpshell") {
		t.Error("dropped the family that IS running")
	}
	// exec and ask_user are dispatcher-side, so they are always described.
	if !strings.Contains(shellOnly, "ask_user") || !strings.Contains(shellOnly, "- exec:") {
		t.Error("built-in tools should always be described")
	}
	// No worktree or Docker → no containment line.
	if strings.Contains(shellOnly, "Containment:") {
		t.Error("should not mention containment with no worktree or Docker")
	}

	all := systemFor([]mcpmgr.MCPTool{
		{Name: "node_query", ServerID: ServerCode},
		{Name: "eval", ServerID: ServerShell},
		{Name: "search", ServerID: ServerDocs},
	}, DockerExec{Dir: "/work", Image: "golang:1.23"}, &Worktree{Path: "/tmp/wt", Branch: "feat"})
	for _, want := range []string{"node_query", "mcpshell", "raglit", "[docs] notes"} {
		if !strings.Contains(all, want) {
			t.Errorf("full tool set: missing %q", want)
		}
	}
	// Should mention both worktree and Docker containment.
	if !strings.Contains(all, "git worktree at /tmp/wt") {
		t.Error("should mention worktree path")
	}
	if !strings.Contains(all, "Docker container") {
		t.Error("should mention Docker containment")
	}
}

// A server that refuses to run must cost you that tool family and nothing else.
// Losing a session because raglit is misconfigured is a worse outcome than
// losing search.
func TestStart_FailingAutostartIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir,
		Servers: []Server{{
			ID: ServerDocs, Command: "sh", Timeout: 5, Autostart: true,
			Args: []string{"-c", "echo 'no project name' >&2; exit 1"},
		}},
		SessionFile: "",
	})
	if err != nil {
		t.Fatalf("a failing server should not fail Start: %v", err)
	}
	defer h.Close()

	st, ok := serverState(h, ServerDocs)
	if !ok {
		t.Fatal("docs missing from Servers()")
	}
	if st.Running {
		t.Error("server exited, but reported running")
	}
	// The reason has to survive: "transport closed" alone sends the user
	// hunting, and the server already said exactly what was wrong.
	if !strings.Contains(st.Err, "no project name") {
		t.Errorf("start error lost the server's own explanation: %q", st.Err)
	}
}

// /rag on for a server that cannot start reports and carries on; the session
// keeps whatever tools it already had.
func TestStartServer_FailureLeavesSessionUsable(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir,
		Servers: []Server{{
			ID: ServerDocs, Command: "definitely-not-a-real-binary-xyz", Timeout: 5,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.StartServer(context.Background(), ServerDocs); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if h.Session == nil || h.Session.Dispatch == nil {
		t.Fatal("session was left unusable by a failed start")
	}
	if st, _ := serverState(h, ServerDocs); st.Err == "" {
		t.Error("failure was not recorded for the UI")
	}
}

// Starting and stopping a server rewrites the tool set, the system prompt and
// the proactive-docs preparer together — they are all derived from the same
// fact and drift apart if any one of them is updated on its own.
func TestStartStopServer_RebuildsToolSet(t *testing.T) {
	if !haveBinary("mcpshell") {
		t.Skip("mcpshell not on PATH")
	}
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir,
		Servers:   []Server{{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", dir}, Timeout: 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if len(h.ToolNames()) != 0 {
		t.Fatalf("nothing autostarted, so there should be no tools: %v", h.ToolNames())
	}
	if err := h.StartServer(context.Background(), ServerShell); err != nil {
		t.Fatal(err)
	}
	if len(h.ToolNames()) == 0 {
		t.Fatal("tools did not appear after a start")
	}
	if !strings.Contains(h.Session.System, "mcpshell") {
		t.Error("system prompt did not pick up the new family")
	}
	if err := h.StopServer(ServerShell); err != nil {
		t.Fatal(err)
	}
	if len(h.ToolNames()) != 0 {
		t.Errorf("tools survived a stop: %v", h.ToolNames())
	}
	if strings.Contains(h.Session.System, "mcpshell") {
		t.Error("system prompt still advertises a stopped family")
	}
}

func haveBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func serverState(h *Harness, id string) (ServerState, bool) {
	for _, st := range h.Servers() {
		if st.ID == id {
			return st, true
		}
	}
	return ServerState{}, false
}

// Rehoist moves workspace + exec + worktree atomically.
func TestRehoist_SwitchesHostToDocker(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        HostExec{Dir: dir},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Start: host exec.
	if h.Workspace() != dir {
		t.Fatalf("workspace = %q, want %q", h.Workspace(), dir)
	}
	if h.IsDocker() {
		t.Error("should start as host exec")
	}

	// Rehoist to docker — same workspace, same worktree (nil).
	h.Rehoist(dir, nil, true)

	if !h.IsDocker() {
		t.Error("should be docker after rehoist")
	}
	de, ok := h.ExecBackend().(DockerExec)
	if !ok {
		t.Fatal("exec backend is not DockerExec")
	}
	if de.Dir != dir {
		t.Errorf("DockerExec.Dir = %q, want %q", de.Dir, dir)
	}
	if de.Image != "golang:1.23" {
		t.Errorf("DockerExec.Image = %q, want golang:1.23", de.Image)
	}

	// Rehoist back to host.
	h.Rehoist(dir, nil, false)
	if h.IsDocker() {
		t.Error("should be host after rehoist off")
	}
	he, ok := h.ExecBackend().(HostExec)
	if !ok {
		t.Fatal("exec backend is not HostExec")
	}
	if he.Dir != dir {
		t.Errorf("HostExec.Dir = %q, want %q", he.Dir, dir)
	}
}

// Rehoist with a worktree moves the workspace path and preserves docker mode.
func TestRehoist_SwitchesWorktree(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "test@test")
	mustRunGit(t, dir, "config", "user.name", "Test")
	os.Create(dir + "/README")
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "init")

	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        HostExec{Dir: dir},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// No worktree initially.
	if wt := h.Worktree(); wt != nil && wt.Branch != "" {
		t.Fatal("should have no worktree at start")
	}

	// Create a worktree and rehoist into it.
	wt, isRepo, err := NewWorktree(dir, nil)
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v, err=%v", isRepo, err)
	}

	h.Rehoist(wt.Path, wt, false)

	if h.Workspace() != wt.Path {
		t.Errorf("workspace = %q, want %q", h.Workspace(), wt.Path)
	}
	if h.Worktree() != wt {
		t.Error("worktree not set")
	}
	he, ok := h.ExecBackend().(HostExec)
	if !ok {
		t.Fatal("not HostExec")
	}
	if he.Dir != wt.Path {
		t.Errorf("HostExec.Dir = %q, want %q", he.Dir, wt.Path)
	}
}

// Rehoist preserves docker mode when switching worktrees.
func TestRehoist_PreservesDockerAcrossWorktree(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "test@test")
	mustRunGit(t, dir, "config", "user.name", "Test")
	os.Create(dir + "/README")
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "init")

	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        DockerExec{Dir: dir, Image: "golang:1.23"},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if !h.IsDocker() {
		t.Fatal("should start as docker")
	}

	wt, isRepo, err := NewWorktree(dir, nil)
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v, err=%v", isRepo, err)
	}

	h.Rehoist(wt.Path, wt, true)

	if !h.IsDocker() {
		t.Error("docker should be preserved after worktree switch")
	}
	de := h.ExecBackend().(DockerExec)
	if de.Dir != wt.Path {
		t.Errorf("DockerExec.Dir = %q, want %q", de.Dir, wt.Path)
	}
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
