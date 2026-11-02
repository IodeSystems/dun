package mounttool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMountSource_NotExist(t *testing.T) {
	_, err := ResolveSource("/nonexistent/path/xyz", "")
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

	resolved, err := ResolveSource("sibling", tmp)
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if resolved != sub {
		t.Errorf("got %s, want %s", resolved, sub)
	}
}

func TestResolveMountSource_Absolute(t *testing.T) {
	tmp := t.TempDir()
	resolved, err := ResolveSource(tmp, "")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
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
	_, err := ResolveSource(f, "")
	if err == nil {
		t.Fatal("expected error for file path")
	}
}
