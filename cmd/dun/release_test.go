package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/update"
)

func TestStagedLine(t *testing.T) {
	res := update.Result{Staged: true, From: "v0.3.0", To: "v0.4.0", Summary: "faster startup"}
	if got, want := stagedLine(res, true), "dun v0.4.0 is ready (was v0.3.0: faster startup); it takes over at the next quiet moment · /upgrade off to stop updates"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	res.Summary = "line one\nline\ttwo\x1b[31m"
	if got := stagedLine(res, false); !strings.Contains(got, "(was v0.3.0: line one line two") || strings.Contains(got, "\n") ||
		!strings.Contains(got, "it takes over when dun next starts") {
		t.Errorf("got %q", got)
	}
}

// /upgrade off persists in the global config and stops the check; on
// brings it back.
func TestUpgradeSwitch(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	line, err := upgradeSwitch(false)
	if err != nil || !loadConfig().NoUpdates || !strings.HasPrefix(line, "updates off — dun no longer checks for new releases · /upgrade on to check again") {
		t.Fatalf("%q %v %v", line, err, loadConfig().NoUpdates)
	}
	if !strings.Contains(line, "it does not update from releases") {
		t.Errorf("a dev build's switch does not say it never updates: %q", line)
	}
	defer func(f func() *update.Updater) { newUpdater = f }(newUpdater)
	newUpdater = func() *update.Updater { return &update.Updater{} }
	if releaseUpdate() != "" {
		t.Error("checked with updates off")
	}
	if line, _ := upgradeSwitch(true); loadConfig().NoUpdates || !strings.HasPrefix(line, "updates on — dun checks for a new release once a day · /upgrade off to stop") {
		t.Fatalf("on: %q", line)
	}
}

// A release check end to end: a signed manifest on a local server names a
// newer dun; it is verified, put over this binary's file, and the running
// dund (the in-process server) is asked to upgrade into it.
func TestReleaseUpdate_StagesAndAsksDund(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("DUN_CHILD", "")
	exe := filepath.Join(t.TempDir(), "dun")
	os.WriteFile(exe, []byte("old build"), 0o755)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := update.PublicKey{ID: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, Key: pub}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dun", Mode: 0o755, Size: 9, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("new build"))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(tb.Bytes())
	m := update.Manifest{Schema: update.Schema, Version: "v0.4.0", Published: "2026-11-01T00:00:00Z", Summary: "faster startup",
		Artifacts: map[string]update.Artifact{"dun": {Version: "v0.4.0", Files: map[string]update.File{
			"linux_amd64": {URL: srv.URL + "/dun_linux_amd64.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(tb.Len())}}}}}
	body, _ := json.Marshal(m)
	mux.HandleFunc("/dun-manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc("/dun-manifest.json.minisig", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(update.Sign(priv, key.ID, body, "timestamp:1")))
	})
	mux.HandleFunc("/dun_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) { w.Write(tb.Bytes()) })

	defer func(f func() *update.Updater) { newUpdater = f }(newUpdater)
	newUpdater = func() *update.Updater {
		return &update.Updater{ManifestURL: srv.URL + "/dun-manifest.json", Keys: []update.PublicKey{key}, Current: "v0.3.0",
			Exe: exe, Home: home, Platform: "linux_amd64", Fetch: &update.Fetcher{AllowHTTP: true}}
	}

	svc, err := dund.New(t.TempDir(), dund.ExecSpawner{Bin: "true"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ln, err := net.Listen("unix", dundcli.Socket(home))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := dund.NewServer(svc, func(net.Conn) (string, error) { return "unix:1", nil })
	type ask struct{ exe, ver string }
	asked := make(chan ask, 1)
	d.SetBuild("v0.3.0", exe, func(e, v string) error { asked <- ask{e, v}; return nil })
	go d.Serve(ln)

	line := releaseUpdate()
	if want := "dun v0.4.0 is ready (was v0.3.0: faster startup); it takes over at the next quiet moment · /upgrade off to stop updates"; line != want {
		t.Fatalf("line %q", line)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new build" {
		t.Fatalf("exe is %q", b)
	}
	select {
	case a := <-asked:
		if a.exe != exe || a.ver != "v0.4.0" {
			t.Fatalf("dund asked for %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dund was not asked to upgrade")
	}
	// Within the day: nothing more.
	if line := releaseUpdate(); line != "" {
		t.Fatalf("second check said %q", line)
	}
}

// A refused download is said, once, and nothing is installed.
func TestReleaseUpdate_Refused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("DUN_CHILD", "")
	exe := filepath.Join(t.TempDir(), "dun")
	os.WriteFile(exe, []byte("old build"), 0o755)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	key := update.PublicKey{ID: [8]byte{9}, Key: pub}
	body := []byte(`{"schema":1,"version":"v0.4.0","published":"2026-11-01T00:00:00Z","artifacts":{}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			w.Write([]byte(update.Sign(other, key.ID, body, "timestamp:1"))) // the right id, another key
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	defer func(f func() *update.Updater) { newUpdater = f }(newUpdater)
	newUpdater = func() *update.Updater {
		return &update.Updater{ManifestURL: srv.URL + "/m.json", Keys: []update.PublicKey{key}, Current: "v0.3.0",
			Exe: exe, Home: home, Platform: "linux_amd64", Fetch: &update.Fetcher{AllowHTTP: true}}
	}
	if line := releaseUpdate(); !strings.HasPrefix(line, "update refused: the manifest's signature: the signature does not match — nothing was installed") {
		t.Fatalf("line %q", line)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old build" {
		t.Fatal("installed after a refused signature")
	}
}

// doctor names how dun and each tool were built.
func TestDoctor_Origins(t *testing.T) {
	d := &doctor{}
	d.build()
	if len(d.found) == 0 || d.found[0].area != "origin" || !strings.HasPrefix(d.found[0].detail, "dun: ") {
		t.Fatalf("%+v", d.found)
	}
	exe, err := os.Executable() // this test binary: a go build with build info
	if err != nil {
		t.Fatal(err)
	}
	d = &doctor{}
	d.toolOrigin("mcpshell", exe)
	if len(d.found) != 1 || !strings.HasPrefix(d.found[0].detail, "mcpshell: ") {
		t.Fatalf("%+v", d.found)
	}
	d = &doctor{}
	d.toolOrigin("mcpshell", "/bin/sh-not-there")
	if len(d.found) != 0 {
		t.Fatalf("a missing file: %+v", d.found)
	}
}

// On a release build, --fix downloads a pinned tool; without --fix the fix
// says so.
func TestDoctor_FixesFromThePin(t *testing.T) {
	defer func(p func(string) (update.Artifact, update.File, bool), i func(string) (string, error)) {
		pinnedTool, installPinned = p, i
	}(pinnedTool, installPinned)
	pinnedTool = func(name string) (update.Artifact, update.File, bool) {
		return update.Artifact{Version: "v0.9.2"}, update.File{}, name == "poly-lsp-mcp"
	}
	got := ""
	installPinned = func(name string) (string, error) { got = name; return "/x/" + name, nil }
	if p, err := (&doctor{fix: true}).install("poly-lsp-mcp"); err != nil || p != "/x/poly-lsp-mcp" || got != "poly-lsp-mcp" {
		t.Fatalf("%q %v %q", p, err, got)
	}
	_, err := (&doctor{}).install("poly-lsp-mcp")
	if fix := toolFix("poly-lsp-mcp", err); fix != "dun doctor --fix (downloads poly-lsp-mcp v0.9.2 from its release; the sha256 is pinned in this build)" {
		t.Fatalf("fix %q", fix)
	}
}
