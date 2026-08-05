package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

func TestResolveMountSource_NotExist(t *testing.T) {
	_, err := resolveMountSource("/nonexistent/path/xyz", "")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestResolveMountSource_RelativeToRepoRoot(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sibling")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveMountSource("sibling", tmp)
	if err != nil {
		t.Fatalf("resolveMountSource: %v", err)
	}
	if resolved != sub {
		t.Errorf("got %s, want %s", resolved, sub)
	}
}

func TestResolveMountSource_Absolute(t *testing.T) {
	tmp := t.TempDir()
	resolved, err := resolveMountSource(tmp, "")
	if err != nil {
		t.Fatalf("resolveMountSource: %v", err)
	}
	if resolved != tmp {
		t.Errorf("got %s, want %s", resolved, tmp)
	}
}

func TestResolveMountSource_NotADirectory(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(f, nil, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveMountSource(f, "")
	if err == nil {
		t.Fatal("expected error for file path")
	}
}

func TestHarness_AddMount_DuplicateName(t *testing.T) {
	h := &Harness{
		cfg: Config{
			Exec: HostExec{Dir: "/tmp"},
			ExtraMounts: []MountSpec{
				{Source: "/a", Name: "dep1"},
			},
		},
	}
	err := h.AddMount("/tmp", "dep1")
	if err == nil {
		t.Fatal("expected error for duplicate mount name")
	}
	if got := err.Error(); got != `mount name "dep1" is already in use` {
		t.Errorf("got %q, want duplicate error", got)
	}
}

func TestHarness_AddMount_WithoutDocker(t *testing.T) {
	tmp := t.TempDir()
	h := &Harness{
		cfg: Config{
			Exec: HostExec{Dir: "/tmp"},
		},
	}
	err := h.AddMount(tmp, "extra")
	if err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if len(h.cfg.ExtraMounts) != 1 {
		t.Errorf("expected 1 mount, got %d", len(h.cfg.ExtraMounts))
	}
	if h.cfg.ExtraMounts[0].Name != "extra" {
		t.Errorf("got name %q, want %q", h.cfg.ExtraMounts[0].Name, "extra")
	}
}

func TestHarness_AddMount_WithDocker(t *testing.T) {
	tmp := t.TempDir()
	h := &Harness{
		cfg: Config{
			Exec: DockerExec{Dir: "/work", Image: "golang:1.23"},
		},
	}
	err := h.AddMount(tmp, "deps")
	if err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	de, ok := h.cfg.Exec.(DockerExec)
	if !ok {
		t.Fatal("exec backend is no longer DockerExec")
	}
	if len(de.ExtraMounts) != 1 {
		t.Errorf("expected 1 mount on DockerExec, got %d", len(de.ExtraMounts))
	}
	if de.ExtraMounts[0].Name != "deps" {
		t.Errorf("got name %q, want %q", de.ExtraMounts[0].Name, "deps")
	}
}

// TestMountTool_RegisteredWithDocker verifies that the mount tool appears in
// the session's tool set when DockerExec is active and cfg.Mount is set.
func TestMountTool_RegisteredWithDocker(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        DockerExec{Dir: dir, Image: "golang:1.23"},
		Mount: func(ctx context.Context, source, name string) (bool, error) {
			return true, nil
		},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	found := false
	for _, td := range h.Session.Tools {
		if td.Function.Name == "mount" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mount tool not registered with DockerExec")
	}
}

// TestMountTool_NotRegisteredWithHost verifies that the mount tool does NOT
// appear when exec is HostExec (no container to mount into).
func TestMountTool_NotRegisteredWithHost(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		Exec:        HostExec{Dir: dir},
		Mount: func(ctx context.Context, source, name string) (bool, error) {
			return true, nil
		},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, td := range h.Session.Tools {
		if td.Function.Name == "mount" {
			t.Fatal("mount tool should not be registered with HostExec")
		}
	}
}

// TestMountTool_DispatcherCallsMountFunc verifies the full dispatch path:
// the tool call reaches the MountFunc callback and the result is correct.
func TestMountTool_DispatcherCallsMountFunc(t *testing.T) {
	dir := t.TempDir()
	mounted := false
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        DockerExec{Dir: dir, Image: "golang:1.23"},
		Mount: func(ctx context.Context, source, name string) (bool, error) {
			mounted = true
			if source != "../agentkit" {
				t.Errorf("source = %q, want ../agentkit", source)
			}
			if name != "agentkit" {
				t.Errorf("name = %q, want agentkit", name)
			}
			return true, nil
		},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	args, _ := json.Marshal(map[string]string{
		"source": "../agentkit",
		"name":   "agentkit",
	})
	var tc llm.ToolCall
	tc.Function.Name = "mount"
	tc.Function.Arguments = string(args)

	result, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !mounted {
		t.Fatal("MountFunc was not called")
	}
	if !strings.Contains(result, "approved") {
		t.Errorf("result = %q, want approved", result)
	}
}

// TestMountTool_DispatcherDeny verifies the denied path.
func TestMountTool_DispatcherDeny(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        DockerExec{Dir: dir, Image: "golang:1.23"},
		Mount: func(ctx context.Context, source, name string) (bool, error) {
			return false, nil
		},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	args, _ := json.Marshal(map[string]string{
		"source": "/tmp",
		"name":   "deps",
	})
	var tc llm.ToolCall
	tc.Function.Name = "mount"
	tc.Function.Arguments = string(args)

	result, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result, "denied") {
		t.Errorf("result = %q, want denied", result)
	}
}

// TestMountTool_DispatcherMissingArgs verifies error handling for missing args.
func TestMountTool_DispatcherMissingArgs(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		DockerImage: "golang:1.23",
		Exec:        DockerExec{Dir: dir, Image: "golang:1.23"},
		Mount: func(ctx context.Context, source, name string) (bool, error) {
			t.Fatal("MountFunc should not be called with empty args")
			return false, nil
		},
		SessionFile: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Empty source.
	var tc llm.ToolCall
	tc.Function.Name = "mount"
	tc.Function.Arguments = `{"source":"","name":"deps"}`
	result, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Errorf("expected error for empty source, got %q", result)
	}

	// Empty name.
	tc.Function.Arguments = `{"source":"/tmp","name":""}`
	result, err = h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Errorf("expected error for empty name, got %q", result)
	}
}

