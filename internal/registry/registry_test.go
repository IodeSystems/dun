package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/update"
)

type key struct {
	pub  update.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t *testing.T) key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	return key{update.PublicKey{ID: id, Key: pub}, priv}
}

func (k key) sign(b []byte) string {
	return update.Sign(k.priv, k.pub.ID, b, "timestamp:1\tfile:index.json\thashed")
}

// The owner's case: one manifest covers raglit 1.0.0 to 1.1.0, 1.1.1 gets
// its own (USER 2026-10-26: less churn); dun's own range rules entries out.
const sample = `{"schema": 1, "published": "2026-10-26T10:00:00Z",
 "extensions": [
  {"id": "raglit", "version": "1.0.0", "dun": ">=0.1.3", "tools": "1.0.0 - 1.1.0", "manifest": {"id": "raglit", "a": 1}},
  {"id": "raglit", "version": "1.1.0", "dun": ">=0.1.3", "tools": "~1.1.1", "manifest": {"id": "raglit", "a": 2}},
  {"id": "raglit", "version": "2.0.0", "dun": ">=0.9.0", "tools": "^1.0.0", "manifest": {"id": "raglit", "a": 3}},
  {"id": "raglit", "version": "0.9.0", "dun": "*", "tools": "^0.2.0", "manifest": {"id": "raglit", "a": 4}}],
 "presets": [
  {"id": "playwright", "version": "1.0.0", "dun": ">=0.1.3", "description": "browser", "command": "npx", "args": ["@playwright/mcp@latest"]},
  {"id": "playwright", "version": "1.1.0", "dun": ">=0.1.3", "description": "browser, headless", "command": "npx", "args": ["@playwright/mcp@latest", "--headless"]},
  {"id": "figma", "version": "1.0.0", "dun": ">=0.5.0", "description": "figma", "url": "http://127.0.0.1:3845/mcp"}],
 "tools": [
  {"id": "raglit", "version": "1.0.0", "dun": ">=0.1.3", "min": "v0.1.0", "recommended": "v0.2.1"}]}`

func TestExtension_RangesPickTheEntry(t *testing.T) {
	ix, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dun, tool, want string // want: the entry's version, "" none
	}{
		{"v0.1.3", "v1.0.0", "1.0.0"},
		{"v0.1.3", "v1.1.0", "1.0.0"},
		{"v0.1.3", "v1.1.1", "1.1.0"},
		{"v0.1.3", "v1.1.7", "1.1.0"},
		{"v0.1.3", "v1.2.0", ""},      // no entry for it: the built-in manifest
		{"v0.9.1", "v1.2.0", "2.0.0"}, // a newer dun reads the newer entry
		{"v0.1.2", "v1.0.0", ""},      // too old a dun for those entries
		{"v0.1.3", "v0.2.1", "0.9.0"}, // npm's 0.x caret: ^0.2.0 is <0.3.0
		{"v0.1.3", "v0.3.0", ""},
		{"dev", "v1.1.1", "2.0.0"},                   // a dev dun reads every entry
		{"v0.1.3", "devel", "1.1.0"},                 // an unknown tool version: the newest for this dun
		{"v0.1.3", "v1.1.1-0.20261024-abc", "1.1.0"}, // a pseudo-version, as its release
	} {
		e, ok := ix.Extension("raglit", tc.dun, tc.tool)
		got := ""
		if ok {
			got = e.Version
		}
		if got != tc.want {
			t.Errorf("dun %s, raglit %s: entry %q, want %q", tc.dun, tc.tool, got, tc.want)
		}
	}
	ps := ix.PresetsFor("v0.1.3")
	if len(ps) != 1 || ps[0].Version != "1.1.0" || len(ps[0].Args) != 2 {
		t.Errorf("presets: %+v", ps)
	}
	if tl, ok := ix.Tool("raglit", "v0.1.3"); !ok || tl.Recommended != "v0.2.1" {
		t.Errorf("tool: %+v %v", tl, ok)
	}
}

func TestParse_RefusesABadEntry(t *testing.T) {
	for name, body := range map[string]string{
		"schema":  `{"schema": 2}`,
		"range":   `{"schema": 1, "extensions": [{"id": "x", "version": "1.0.0", "dun": ">=0.1", "tools": "not a range", "manifest": {}}]}`,
		"version": `{"schema": 1, "presets": [{"id": "x", "version": "one", "dun": "*", "description": "", "command": "x"}]}`,
		"both":    `{"schema": 1, "presets": [{"id": "x", "version": "1.0.0", "dun": "*", "description": "", "command": "x", "url": "http://x"}]}`,
		"unknown": `{"schema": 1, "plugins": []}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The store keeps a verified index, reads it back verified, fetches it once
// a day at most, refuses one signed by another key or older than the one
// kept, and refuses a cached file changed on disk.
func TestStore(t *testing.T) {
	k, other := newKey(t), newKey(t)
	body, sig := []byte(sample), k.sign([]byte(sample))
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			w.Write([]byte(sig))
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	now := time.Now()
	s := &Store{Dir: t.TempDir(), URL: srv.URL + "/registry/v1/index.json", Keys: []update.PublicKey{k.pub},
		Fetch: &update.Fetcher{AllowHTTP: true}, MaxAge: DefaultMaxAge, Now: func() time.Time { return now }}
	if _, err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty store: %v", err)
	}
	ix, changed, err := s.Refresh(context.Background())
	if err != nil || !changed || len(ix.Extensions) != 4 {
		t.Fatalf("first refresh: %v %v", changed, err)
	}
	if _, changed, err := s.Refresh(context.Background()); err != nil || changed || hits != 2 {
		t.Fatalf("a fresh cache was fetched again: hits %d, %v %v", hits, changed, err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	// A day later, an older index: refused, the kept one stays.
	now = now.Add(25 * time.Hour)
	body = []byte(strings.Replace(sample, "2026-10-26T10:00:00Z", "2026-10-01T10:00:00Z", 1))
	sig = k.sign(body)
	if ix, _, err := s.Refresh(context.Background()); err == nil || ix.Published != "2026-10-26T10:00:00Z" {
		t.Errorf("an older index replaced the kept one: %v", err)
	}

	// Signed by a key this build does not trust: refused.
	now = now.Add(25 * time.Hour)
	body = []byte(strings.Replace(sample, "2026-10-26", "2026-10-27", 1))
	sig = other.sign(body)
	if _, changed, err := s.Refresh(context.Background()); err == nil || changed {
		t.Errorf("another key's index was kept: %v", err)
	}

	// The cached file changed on disk: Load refuses it.
	if err := os.WriteFile(filepath.Join(s.Dir, indexFile), []byte(strings.Replace(sample, ">=0.1.3", "*", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("a changed cache verified")
	}
}
