package ragd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// modelsServer lists ids at /models and counts the listings.
func modelsServer(t *testing.T, ids string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			hits.Add(1)
			w.Write([]byte(`{"data":[` + ids + `]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func cfgFor(url string) dunconfig.DunConfig {
	return dunconfig.DunConfig{Provider: "gw", Providers: []dunconfig.ProviderConfig{{Name: "gw", URL: url, Key: "k"}}}
}

func TestResolveServedDefaultOnWhenListed(t *testing.T) {
	var hits atomic.Int32
	s := modelsServer(t, `{"id":"`+dunconfig.DefaultEmbedModel+`"}`, &hits)
	r := ResolveServed(context.Background(), cfgFor(s.URL), t.TempDir())
	if r.Models.Embed.Model != dunconfig.DefaultEmbedModel || !r.EmbedDefault {
		t.Fatalf("embed = %+v default=%v", r.Models.Embed, r.EmbedDefault)
	}
}

func TestResolveServedOffWhenNotListedAndCached(t *testing.T) {
	var hits atomic.Int32
	s := modelsServer(t, `{"id":"openai/gpt-4o"}`, &hits)
	caps := t.TempDir()
	r := ResolveServed(context.Background(), cfgFor(s.URL), caps)
	if r.Models.Embed.Model != "" {
		t.Fatalf("embed on: %+v", r.Models.Embed)
	}
	n := 0
	for _, s := range r.Said {
		if strings.HasPrefix(s, "embed off: gw has no "+dunconfig.DefaultEmbedModel+"; search uses full-text only") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("off line said %d times: %q", n, r.Said)
	}
	ResolveServed(context.Background(), cfgFor(s.URL), caps)
	if hits.Load() != 1 {
		t.Fatalf("listed %d times, want 1 (cached)", hits.Load())
	}
}

func TestResolveServedListingFailsLeavesOn(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	r := ResolveServed(context.Background(), cfgFor(s.URL), t.TempDir())
	if r.Models.Embed.Model != dunconfig.DefaultEmbedModel {
		t.Fatalf("embed = %+v", r.Models.Embed)
	}
}

func TestResolveServedExplicitFacetNotChecked(t *testing.T) {
	var hits atomic.Int32
	s := modelsServer(t, `{"id":"other"}`, &hits)
	cfg := cfgFor(s.URL)
	cfg.Facets = map[string]dunconfig.Facet{"embed": {Model: "text-embedding-3-small"}}
	r := ResolveServed(context.Background(), cfg, t.TempDir())
	if r.Models.Embed.Model != "text-embedding-3-small" || r.EmbedDefault || hits.Load() != 0 {
		t.Fatalf("embed = %+v default=%v hits=%d", r.Models.Embed, r.EmbedDefault, hits.Load())
	}
}

type failVec struct {
	err   error
	calls atomic.Int32
}

func (f *failVec) Embed(context.Context, string, []string) ([][]float32, error) {
	f.calls.Add(1)
	return nil, f.err
}

func TestEmbedGuardRuntimeRefusalTurnsOff(t *testing.T) {
	var hits atomic.Int32
	s := modelsServer(t, `{"id":"`+dunconfig.DefaultEmbedModel+`"}`, &hits)
	caps := t.TempDir()
	role := raglit.ModelRole{BaseURL: s.URL, Model: dunconfig.DefaultEmbedModel}
	var logs []string
	g := &embedGuard{role: role, capsDir: caps, logf: func(f string, a ...any) { logs = append(logs, f) }}
	v := &failVec{err: errors.New(`llm: status 400: {"error":{"message":"Model local-nomic-embed-text does not exist","code":400}}`)}
	rt := embedRoute{ingest: v, query: v, guard: g}
	if _, err := rt.Embed(context.Background(), role.Model, []string{"a"}); err == nil || !g.isOff() {
		t.Fatalf("first call err=%v off=%v", err, g.isOff())
	}
	for i := 0; i < 3; i++ {
		if _, err := rt.Embed(context.Background(), role.Model, []string{"a"}); !errors.Is(err, errEmbedOff) {
			t.Fatalf("later call err=%v", err)
		}
	}
	if v.calls.Load() != 1 || len(logs) != 1 {
		t.Fatalf("requests=%d logs=%d, want 1 and 1", v.calls.Load(), len(logs))
	}
	// The next start reads the record: off, with no listing.
	if served, known := embedServed(context.Background(), caps, role); !known || served || hits.Load() != 0 {
		t.Fatalf("served=%v known=%v listings=%d", served, known, hits.Load())
	}
}

func TestEmbedGuardOtherErrorsKept(t *testing.T) {
	g := &embedGuard{capsDir: t.TempDir(), logf: func(string, ...any) {}}
	v := &failVec{err: errors.New("llm: status 500: boom")}
	rt := embedRoute{ingest: v, query: v, guard: g}
	rt.Embed(context.Background(), "m", []string{"a"})
	if g.isOff() {
		t.Fatal("a 500 turned embed off")
	}
	// An explicit facet has no guard: a 400 is passed through each time.
	v2 := &failVec{err: errors.New("llm: status 400: Model x does not exist")}
	rt = embedRoute{ingest: v2, query: v2}
	for i := 0; i < 2; i++ {
		if _, err := rt.Embed(context.Background(), "x", nil); err == nil || errors.Is(err, errEmbedOff) {
			t.Fatalf("err = %v", err)
		}
	}
	if v2.calls.Load() != 2 {
		t.Fatalf("calls = %d", v2.calls.Load())
	}
}
