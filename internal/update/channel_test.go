package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A channel switch installs the newest release whatever this build's
// version: a source build's describe, or a newer version (an explicit
// switch may go back). The replay check on the manifest stays.
func TestUpdater_Switch(t *testing.T) {
	for _, cur := range []string{"v0.4.1-30-gabcdef0", "v0.5.0", "dev"} {
		srv, key, exe, home := setup(t)
		release{srv: srv, key: key, version: "v0.4.1", body: "v0.4.1 binary"}.publish(t)
		res, err := newUpdater(srv, []PublicKey{key.pub}, cur, exe, home).Switch(context.Background())
		if err != nil || !res.Staged || res.From != cur || res.To != "v0.4.1" {
			t.Fatalf("%s: %+v %v", cur, res, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "v0.4.1 binary" {
			t.Fatalf("%s: exe is %q", cur, b)
		}
		if st := loadState(StateFile(home)); st.Version != "v0.4.1" || st.Staged != "v0.4.1" {
			t.Fatalf("%s: state %+v", cur, st)
		}
	}
	// A replay: a manifest older than one seen before is refused.
	srv, key, exe, home := setup(t)
	release{srv: srv, key: key, version: "v0.4.0", body: "new"}.publish(t)
	if err := saveState(StateFile(home), State{Version: "v0.5.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := newUpdater(srv, []PublicKey{key.pub}, "v0.4.1-30-gabcdef0", exe, home).Switch(context.Background()); err == nil || !strings.Contains(err.Error(), "seen before") {
		t.Fatalf("replay: %v", err)
	}
	// A bad signature.
	if _, err := newUpdater(srv, []PublicKey{newKey(t).pub}, "v0.4.1-30-gabcdef0", exe, t.TempDir()).Switch(context.Background()); err == nil || !IsVerify(err) {
		t.Fatalf("bad signature: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v0.3.0 binary" {
		t.Fatalf("exe replaced: %q", b)
	}
	// A target directory that does not exist yet (~/.local/bin) is made.
	srv, key, _, home = setup(t)
	release{srv: srv, key: key, version: "v0.4.1", body: "v0.4.1 binary"}.publish(t)
	dst := filepath.Join(t.TempDir(), "local", "bin", "dun")
	if _, err := newUpdater(srv, []PublicKey{key.pub}, "dev", dst, home).Switch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "v0.4.1 binary" {
		t.Fatalf("dst is %q", b)
	}
}

func TestSwitchTarget(t *testing.T) {
	cache, inst := t.TempDir(), t.TempDir()
	t.Setenv("DUN_BIN", "")
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("DUN_INSTALL_DIR", inst)
	lb := filepath.Join(cache, "dun", "dun")
	if LauncherBin() != lb || InstallPath() != filepath.Join(inst, "dun") {
		t.Fatalf("%s %s", LauncherBin(), InstallPath())
	}
	os.MkdirAll(filepath.Dir(lb), 0o755)
	os.WriteFile(lb, []byte("x"), 0o755)
	if got, l := SwitchTarget(lb); !l || got != filepath.Join(inst, "dun") {
		t.Fatalf("launcher: %s %v", got, l)
	}
	other := filepath.Join(t.TempDir(), "dun")
	if got, l := SwitchTarget(other); l || got != other {
		t.Fatalf("own file: %s %v", got, l)
	}
}

func TestShadow(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(a, "dun"), []byte("#!/usr/bin/env bash\n# dun launcher — what `make install-launcher` puts on your PATH.\n"), 0o755)
	os.WriteFile(filepath.Join(b, "dun"), []byte("binary"), 0o755)
	os.WriteFile(filepath.Join(c, "dun"), []byte("not executable"), 0o644)
	target := filepath.Join(b, "dun")
	path := func(d ...string) string { return strings.Join(d, string(os.PathListSeparator)) }
	if first, on := Shadow(target, path(c, b, a)); first != "" || !on {
		t.Errorf("target first: %q %v", first, on)
	}
	if first, on := Shadow(target, path(a, b)); first != filepath.Join(a, "dun") || !on || !IsLauncher(first) {
		t.Errorf("launcher first: %q %v", first, on)
	}
	if first, on := Shadow(target, path(a)); first != filepath.Join(a, "dun") || on {
		t.Errorf("not on PATH: %q %v", first, on)
	}
	if first, on := Shadow(target, path(c)); first != "" || on {
		t.Errorf("nothing on PATH: %q %v", first, on)
	}
	if IsLauncher(target) {
		t.Error("a binary is the launcher")
	}
}
