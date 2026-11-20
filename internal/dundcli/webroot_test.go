package dundcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWebRoot(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	def := filepath.Join(xdg, "dun", "web")

	if got := resolveWebRoot("", false); got != "" {
		t.Errorf("default dir missing: got %q, want empty", got)
	}
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveWebRoot("", false); got != "" {
		t.Errorf("default dir without index.html: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(def, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveWebRoot("", false); got != def {
		t.Errorf("default dir with index.html: got %q, want %q", got, def)
	}
	if got := resolveWebRoot("/explicit", false); got != "/explicit" {
		t.Errorf("explicit flag: got %q", got)
	}
	if got := resolveWebRoot("", true); got != "" {
		t.Errorf("embedded client: got %q, want empty", got)
	}
}
