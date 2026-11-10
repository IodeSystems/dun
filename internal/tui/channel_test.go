package tui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/update"
)

// releaseServer serves a signed manifest naming dun version for
// linux_amd64, whose binary's content is "release build"; it returns the
// manifest URL and the key.
func releaseServer(t *testing.T, version string) (string, update.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := update.PublicKey{ID: [8]byte{8, 7, 6, 5, 4, 3, 2, 1}, Key: pub}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dun", Mode: 0o755, Size: 13, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("release build"))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(tb.Bytes())
	m := update.Manifest{Schema: update.Schema, Version: version, Published: "2026-11-01T00:00:00Z",
		Artifacts: map[string]update.Artifact{"dun": {Version: version, Files: map[string]update.File{
			"linux_amd64": {URL: srv.URL + "/dun_linux_amd64.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(tb.Len())}}}}}
	body, _ := json.Marshal(m)
	mux.HandleFunc("/dun-manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc("/dun-manifest.json.minisig", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(update.Sign(priv, key.ID, body, "timestamp:1")))
	})
	mux.HandleFunc("/dun_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) { w.Write(tb.Bytes()) })
	return srv.URL + "/dun-manifest.json", key
}

// stubChannel points the switch at a local signed release and at exe as
// this binary; every path is a temp dir.
func stubChannel(t *testing.T, isRelease bool, exe, manifest string, key update.PublicKey) {
	t.Helper()
	oldRel, oldExe, oldSw, oldAsk, oldVer := channelIsRelease, channelExe, newSwitcher, askDundUpgrade, buildinfo.Version
	t.Cleanup(func() {
		channelIsRelease, channelExe, newSwitcher, askDundUpgrade, buildinfo.Version = oldRel, oldExe, oldSw, oldAsk, oldVer
	})
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	buildinfo.Version = "v0.4.1-30-gabcdef0"
	channelIsRelease = func() bool { return isRelease }
	channelExe = func() (string, error) { return exe, nil }
	newSwitcher = func(target string) (*update.Updater, error) {
		return &update.Updater{ManifestURL: manifest, Keys: []update.PublicKey{key}, Current: buildinfo.Version, Exe: target,
			Home: home, Platform: "linux_amd64", Fetch: &update.Fetcher{AllowHTTP: true}}, nil
	}
	askDundUpgrade = func(string, string) bool { return false }
}

func convoLast(m *tuiModel) string { return ansi.Strip(m.convo[len(m.convo)-1].view()) }

// A source build (make install) switches to release: the release goes over
// its own file, older than this build, and both versions are said.
func TestChannel_SourceToRelease(t *testing.T) {
	bin := t.TempDir()
	exe := filepath.Join(bin, "dun")
	os.WriteFile(exe, []byte("source build"), 0o755)
	t.Setenv("DUN_BIN", filepath.Join(t.TempDir(), "dun"))
	t.Setenv("PATH", bin)
	url, key := releaseServer(t, "v0.4.1")
	stubChannel(t, false, exe, url, key)

	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	cmd := upgradeSlash(&m, []string{"--channel=release"})
	if cmd == nil {
		t.Fatal("no switch")
	}
	m.channelDone(cmd().(channelMsg))
	got := convoLast(&m)
	if !strings.Contains(got, "dun release v0.4.1 is installed at "+exe+" (was the source build v0.4.1-30-gabcdef0)") ||
		strings.Contains(got, "warning") || !strings.Contains(got, "plain /upgrade follows releases") {
		t.Fatalf("got:\n%s", got)
	}
	if b, _ := os.ReadFile(exe); string(b) != "release build" {
		t.Fatalf("exe is %q", b)
	}
}

// The launcher's binary: the release goes to install.sh's path, the
// launcher's binary and script stay, and the script coming first on PATH is
// named.
func TestChannel_LauncherToRelease(t *testing.T) {
	cache, gobin, inst := t.TempDir(), t.TempDir(), t.TempDir()
	exe := filepath.Join(cache, "dun")
	os.WriteFile(exe, []byte("source build"), 0o755)
	script := filepath.Join(gobin, "dun")
	os.WriteFile(script, []byte("#!/usr/bin/env bash\n# dun launcher — what `make install-launcher` puts on your PATH.\n"), 0o755)
	t.Setenv("DUN_BIN", exe)
	t.Setenv("DUN_INSTALL_DIR", inst)
	t.Setenv("PATH", gobin+string(os.PathListSeparator)+inst)
	url, key := releaseServer(t, "v0.4.1")
	stubChannel(t, false, exe, url, key)

	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.channelDone(upgradeSlash(&m, []string{"--channel", "release"})().(channelMsg))
	got := convoLast(&m)
	target := filepath.Join(inst, "dun")
	if !strings.Contains(got, "installed at "+target) || !strings.Contains(got, "warning: the launcher at "+script+" comes first on PATH") ||
		!strings.Contains(got, "it was not removed") {
		t.Fatalf("got:\n%s", got)
	}
	if b, _ := os.ReadFile(target); string(b) != "release build" {
		t.Fatalf("target is %q", b)
	}
	if b, _ := os.ReadFile(exe); string(b) != "source build" {
		t.Fatalf("the launcher's binary was replaced: %q", b)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the launcher script: %v", err)
	}
}

// A refused manifest installs nothing and says so.
func TestChannel_Refused(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "dun")
	os.WriteFile(exe, []byte("source build"), 0o755)
	t.Setenv("DUN_BIN", filepath.Join(t.TempDir(), "dun"))
	url, _ := releaseServer(t, "v0.4.1")
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	stubChannel(t, false, exe, url, update.PublicKey{Key: other})

	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.channelDone(upgradeSlash(&m, []string{"--channel=release"})().(channelMsg))
	if got := convoLast(&m); !strings.Contains(got, "channel: not switched:") || !strings.Contains(got, "nothing was installed") {
		t.Fatalf("got:\n%s", got)
	}
	if b, _ := os.ReadFile(exe); string(b) != "source build" {
		t.Fatalf("exe replaced: %q", b)
	}
}

// release → source says make install; an unknown channel is refused; the
// channel this dun is on is plain /upgrade. None of them switch.
func TestChannel_OtherCases(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "dun")
	os.WriteFile(exe, []byte("release build"), 0o755)
	stubChannel(t, true, exe, "http://127.0.0.1:1/none", update.PublicKey{})
	oldTools := runTools
	defer func() { runTools = oldTools }()
	runTools = func(bool) ([]toolState, error) { return nil, nil }
	buildinfo.Version = "v0.4.1"
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")

	if cmd := upgradeSlash(&m, []string{"--channel=source"}); cmd != nil || !strings.Contains(convoLast(&m), "run `make install` in your dun checkout (nothing was downloaded or built)") {
		t.Errorf("release → source: %q", convoLast(&m))
	}
	for _, args := range [][]string{{"--channel=nightly"}, {"--channel"}, {"--channel="}} {
		if cmd := upgradeSlash(&m, args); cmd != nil || !strings.Contains(convoLast(&m), "--channel=release or --channel=source") {
			t.Errorf("%v: %q", args, convoLast(&m))
		}
	}
	cmd := upgradeSlash(&m, []string{"--channel=release"})
	if _, ok := cmd().(toolsMsg); !ok || !strings.Contains(ansi.Strip(m.convo[len(m.convo)-2].view()), "already on the release channel (v0.4.1)") {
		t.Errorf("release on release: %q", ansi.Strip(m.convo[len(m.convo)-2].view()))
	}
	channelIsRelease = func() bool { return false }
	cmd = upgradeSlash(&m, []string{"--channel=source"})
	if _, ok := cmd().(toolsMsg); !ok || !strings.Contains(ansi.Strip(m.convo[len(m.convo)-2].view()), "already on the source channel") {
		t.Errorf("source on source: %q", ansi.Strip(m.convo[len(m.convo)-2].view()))
	}
	if b, _ := os.ReadFile(exe); string(b) != "release build" {
		t.Fatalf("exe replaced: %q", b)
	}
}
