package mcpserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// exe writes an executable script at p.
func exe(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A home with no tools and a PATH that has every managed tool: PATH is not
// searched, so all three are missing.
func toolHome(t *testing.T) (home, path string) {
	t.Helper()
	home, path = t.TempDir(), t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("PATH", path)
	for _, s := range release.Supported {
		t.Setenv(ToolEnv(s.Name), "")
		exe(t, filepath.Join(path, s.Name))
	}
	return home, path
}

// The resolution order: an override (env over mcp.json), else the pinned
// copy, else the newest kept copy at or past Min, else missing. PATH and
// dun's directory are never searched for a managed tool.
func TestResolveTool_Order(t *testing.T) {
	home, _ := toolHome(t)
	pin := PinVersion("mcpshell")
	r := ResolveTool("mcpshell")
	if r.Source != SourceMissing || r.Path != "" || r.Pin != pin {
		t.Fatalf("PATH has mcpshell, but PATH is not searched: %+v", r)
	}
	_, err := FindCommand("mcpshell")
	var miss *MissingError
	if !errors.As(err, &miss) || !strings.Contains(err.Error(), "no eval tool") || !strings.Contains(err.Error(), "dun doctor") {
		t.Fatalf("missing mcpshell: %v", err)
	}
	// The copy on PATH is named as unused, with how to use it, in one line.
	onPath := filepath.Join(os.Getenv("PATH"), "mcpshell")
	if want := "; " + onPath + " on PATH is not used (set DUN_MCPSHELL or tools.mcpshell.path to use it)"; !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "\n") {
		t.Errorf("missing with a copy on PATH: %v", err)
	}
	if got := PinnedPath("mcpshell", "v1.0.0"); got != filepath.Join(home, "tools", "mcpshell", "v1.0.0", runtime.GOOS+"-"+runtime.GOARCH, "mcpshell") {
		t.Errorf("PinnedPath %s", got)
	}

	// A kept copy at or past Min runs when the pin's is not there; one
	// below Min does not.
	s, _ := SupportedTool("mcpshell")
	exe(t, PinnedPath("mcpshell", "v0.0.1"))
	if r := ResolveTool("mcpshell"); r.Source != SourceMissing {
		t.Errorf("a copy below Min: %+v", r)
	}
	exe(t, PinnedPath("mcpshell", s.Min))
	if r := ResolveTool("mcpshell"); r.Source != SourcePinned || r.Version != s.Min || !r.Stale() {
		t.Errorf("a kept copy: %+v", r)
	}

	// The pin's copy.
	want := exe(t, PinnedPath("mcpshell", pin))
	if r := ResolveTool("mcpshell"); r.Source != SourcePinned || r.Path != want || r.Version != pin || r.Stale() {
		t.Errorf("pinned: %+v", r)
	}
	if p, err := FindCommand("mcpshell"); err != nil || p != want {
		t.Errorf("FindCommand: %q %v", p, err)
	}

	// tools.<name>.path in $DUN_HOME/mcp.json wins over the pinned copy.
	cfg := exe(t, filepath.Join(t.TempDir(), "my-mcpshell"))
	b, _ := json.Marshal(map[string]any{"tools": map[string]any{"mcpshell": map[string]string{"path": cfg}}})
	if err := os.WriteFile(UserServersFile(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ResolveTool("mcpshell"); r.Source != SourceOverride || r.Path != cfg || !strings.HasPrefix(r.Via, "tools.mcpshell.path in ") {
		t.Errorf("config override: %+v", r)
	}
	// $DUN_MCPSHELL wins over the config.
	env := exe(t, filepath.Join(t.TempDir(), "env-mcpshell"))
	t.Setenv("DUN_MCPSHELL", env)
	if r := ResolveTool("mcpshell"); r.Source != SourceOverride || r.Path != env || r.Via != "$DUN_MCPSHELL" {
		t.Errorf("env override: %+v", r)
	}
	if p, err := FindCommand("mcpshell"); err != nil || p != env {
		t.Errorf("FindCommand with an override: %q %v", p, err)
	}
	// An override that is not an executable is missing, with the reason; the
	// pinned copy does not stand in for it.
	t.Setenv("DUN_MCPSHELL", filepath.Join(t.TempDir(), "nope"))
	if r := ResolveTool("mcpshell"); r.Source != SourceMissing || !strings.Contains(r.Why, "$DUN_MCPSHELL is ") {
		t.Errorf("a broken override: %+v", r)
	}
	if _, err := FindCommand("mcpshell"); !errors.As(err, &miss) || !strings.Contains(err.Error(), "not an executable file") {
		t.Errorf("a broken override: %v", err)
	}

	// A server command that is a path is used as it is; a command dun does
	// not manage is looked for on PATH.
	if p, err := FindCommand("./x/y"); err != nil || p != "./x/y" {
		t.Errorf("a path is used as it is: %q, %v", p, err)
	}
	other := exe(t, filepath.Join(os.Getenv("PATH"), "some-lsp"))
	if p, err := FindCommand("some-lsp"); err != nil || p != other {
		t.Errorf("an unmanaged command on PATH: %q, %v", p, err)
	}
	if ToolEnv("poly-lsp-mcp") != "DUN_POLY_LSP_MCP" || ToolEnv("raglit") != "DUN_RAGLIT" {
		t.Error("ToolEnv names")
	}
}

// tarball is a tar.gz with one file, name.
func tarball(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeRelease makes this a release build whose manifest pins mcpshell
// v9.9.9 at an httptest server that counts its downloads. sum is the
// sha256 the manifest pins ("" for the tarball's own).
func fakeRelease(t *testing.T, release bool, sum string) (*atomic.Int32, chan struct{}) {
	t.Helper()
	body := tarball(t, "mcpshell", "#!/bin/sh\necho fetched\n")
	hits := &atomic.Int32{}
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	if sum == "" {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	plat := update.Platform(runtime.GOOS, runtime.GOARCH)
	m := map[string]any{"schema": 1, "artifacts": map[string]any{"mcpshell": map[string]any{
		"version": "v9.9.9",
		"files":   map[string]any{plat: map[string]any{"url": srv.URL + "/mcpshell.tar.gz", "sha256": sum, "size": len(body)}},
	}}}
	deps, _ := json.Marshal(m)
	oldRel, oldDeps, oldF := releaseBuild, pinnedDeps, toolFetcher
	t.Cleanup(func() { releaseBuild, pinnedDeps, toolFetcher = oldRel, oldDeps, oldF })
	releaseBuild = func() bool { return release }
	pinnedDeps = func() []byte { return deps }
	toolFetcher = &update.Fetcher{AllowHTTP: true}
	return hits, gate
}

// A release build fetches a missing pinned copy on first need, saying so;
// two sessions at once download it once.
func TestEnsureCommand_FetchesOnce(t *testing.T) {
	toolHome(t)
	hits, gate := fakeRelease(t, true, "")
	if pin := PinVersion("mcpshell"); pin != "v9.9.9" {
		t.Fatalf("pin %q", pin)
	}
	var mu sync.Mutex
	var said []string
	say := func(s string) { mu.Lock(); said = append(said, s); mu.Unlock() }
	var wg sync.WaitGroup
	paths := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = EnsureCommand(context.Background(), "mcpshell", say)
		}()
	}
	for hits.Load() == 0 {
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()
	want := PinnedPath("mcpshell", "v9.9.9")
	for i := range 2 {
		if errs[i] != nil || paths[i] != want {
			t.Errorf("session %d: %q %v", i, paths[i], errs[i])
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("downloaded %d times", n)
	}
	if b, _ := os.ReadFile(want); string(b) != "#!/bin/sh\necho fetched\n" {
		t.Errorf("installed %q", b)
	}
	if len(said) != 2 || said[0] != "fetching mcpshell v9.9.9 …" || said[1] != "fetched mcpshell v9.9.9" {
		t.Errorf("said %q", said)
	}
	if r := ResolveTool("mcpshell"); r.Source != SourcePinned || r.Path != want {
		t.Errorf("after: %+v", r)
	}
}

// A sha256 that does not match is refused: nothing is installed, the
// session gets a MissingError with the reason, and it was said.
func TestEnsureCommand_RefusesABadSum(t *testing.T) {
	toolHome(t)
	_, gate := fakeRelease(t, true, strings.Repeat("0", 64))
	close(gate)
	var said []string
	_, err := EnsureCommand(context.Background(), "mcpshell", func(s string) { said = append(said, s) })
	var miss *MissingError
	if !errors.As(err, &miss) || !strings.Contains(miss.Why, "sha256") {
		t.Fatalf("a bad sum: %v", err)
	}
	if _, serr := os.Stat(PinnedPath("mcpshell", "v9.9.9")); serr == nil {
		t.Error("a refused download was installed")
	}
	if len(said) != 2 || !strings.HasPrefix(said[1], "fetching mcpshell v9.9.9 failed: ") || !strings.Contains(said[1], "sha256") {
		t.Errorf("said %q", said)
	}
	ents, _ := os.ReadDir(filepath.Dir(PinnedPath("mcpshell", "v9.9.9")))
	if len(ents) != 0 {
		t.Errorf("temp files left: %v", ents)
	}
}

// A build that is not a release never fetches: the tool is missing, and
// nothing is downloaded.
func TestEnsureCommand_NonReleaseDoesNotFetch(t *testing.T) {
	toolHome(t)
	hits, gate := fakeRelease(t, false, "")
	close(gate)
	_, err := EnsureCommand(context.Background(), "mcpshell", func(s string) { t.Errorf("said %q", s) })
	var miss *MissingError
	if !errors.As(err, &miss) || miss.Why != "" {
		t.Fatalf("non-release: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("a non-release build downloaded")
	}
	if _, _, ok := PinnedDownload("mcpshell"); ok {
		t.Error("a non-release build has a pinned download")
	}
}

// A session waiting for another dun's fetch stops waiting when its ctx
// ends (closed, or the server switched off): the tool is missing, not hung.
func TestEnsureCommand_WaitHonoursContext(t *testing.T) {
	toolHome(t)
	_, gate := fakeRelease(t, true, "")
	defer close(gate)
	dir := filepath.Join(ToolsDir(), "mcpshell")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(filepath.Join(dir, ".lock")) // another dun fetching
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err = EnsureCommand(ctx, "mcpshell", nil)
	var miss *MissingError
	if !errors.As(err, &miss) || time.Since(t0) > 5*time.Second {
		t.Fatalf("after %s: %v", time.Since(t0), err)
	}
}
