package dun

import (
	"os"
	"path/filepath"
	"testing"
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

	// Relative path resolved against repoRoot.
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
	// Check the exec backend was reconfigured with the mount.
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

