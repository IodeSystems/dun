package dunconfig

import (
	"os"
	"strings"
	"testing"
)

func facetCfg() DunConfig {
	return DunConfig{Provider: "gw", URL: "http://gw:4000", Key: "k1", Model: "old-chat",
		VisionModel: "old-vl", VisionProvider: "far",
		Providers: []ProviderConfig{{Name: "gw", URL: "http://gw:4000", Key: "k1"}, {Name: "far", URL: "https://far", Key: "k2"}}}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// flag > env > facets > old field > default, for the model and for the
// endpoint, field by field.
func TestResolvePrecedence(t *testing.T) {
	c := facetCfg()
	c.Facets = map[string]Facet{FacetMain: {Provider: "far", Model: "fc-chat", Effort: "low"}}
	env := envOf(map[string]string{"DUN_MODEL": "env-chat"})

	r := c.Resolve(FacetMain, nil, nil)
	if r.Model != "fc-chat" || r.Provider != "far" || r.URL != "https://far" || r.Key != "k2" || r.Effort != "low" || r.Source != SourceConfig {
		t.Fatalf("facets: %+v", r)
	}
	r = c.Resolve(FacetMain, nil, env)
	if r.Model != "env-chat" || r.Source != SourceEnv || r.URL != "https://far" {
		t.Fatalf("env model keeps the facet's endpoint: %+v", r)
	}
	r = c.Resolve(FacetMain, Overrides{FacetMain: {Model: "flag-chat", URL: "http://gw:4000/"}}, env)
	if r.Model != "flag-chat" || r.Source != SourceFlag || r.Provider != "gw" || r.Key != "k1" {
		t.Fatalf("flag: %+v", r)
	}
	r = c.Resolve(FacetMain, Overrides{FacetMain: {Key: "flag-key"}}, envOf(map[string]string{"DUN_LLM_KEY": "env-key"}))
	if r.Key != "flag-key" {
		t.Fatalf("flag key: %+v", r)
	}
	r = c.Resolve(FacetMain, nil, envOf(map[string]string{"DUN_URL": "http://elsewhere"}))
	if r.Provider != "elsewhere" || r.Key != "" || r.Model != "fc-chat" {
		t.Fatalf("an unsaved env URL gets no saved key: %+v", r)
	}

	// Child: flags over facets; nothing named is main's.
	if r := c.Resolve(FacetChild, nil, nil); r.Model != "" || r.Provider != "" || r.Source != SourceDefault {
		t.Fatalf("child default: %+v", r)
	}
	c.Facets[FacetChild] = Facet{Model: "kid", Effort: "off"}
	if r := c.Resolve(FacetChild, nil, nil); r.Model != "kid" || r.Provider != "" || r.URL != "" || r.Effort != "off" {
		t.Fatalf("child facet with no provider stays on main's: %+v", r)
	}
	r = c.Resolve(FacetChild, Overrides{FacetChild: {Provider: "gw", Model: "flag-kid"}}, nil)
	if r.Model != "flag-kid" || r.URL != "http://gw:4000" || r.Effort != "off" || r.Source != SourceFlag {
		t.Fatalf("child flag: %+v", r)
	}
}

// A config from before facets means what it meant: model and vision_model.
func TestResolveOldConfigFallback(t *testing.T) {
	c := facetCfg()
	r := c.Resolve(FacetMain, nil, nil)
	if r.Model != "old-chat" || r.Provider != "gw" || r.Key != "k1" || r.Source != SourceOld {
		t.Fatalf("main from model: %+v", r)
	}
	v := c.Resolve(FacetVision, nil, nil)
	if v.Model != "old-vl" || v.URL != "https://far" || v.Source != SourceOld {
		t.Fatalf("vision from vision_model: %+v", v)
	}
	c.Facets = map[string]Facet{FacetVision: {Model: "new-vl"}}
	if v := c.Resolve(FacetVision, nil, nil); v.Model != "new-vl" || v.URL != "https://far" || v.Source != SourceConfig {
		t.Fatalf("facets.vision over vision_model (provider from the old field below it): %+v", v)
	}
	e := c.Resolve(FacetEmbed, nil, nil)
	if e.Model != DefaultEmbedModel || e.Provider != "gw" || e.Source != SourceDefault {
		t.Fatalf("embed default: %+v", e)
	}
	if s := c.Resolve(FacetSegment, nil, nil); s.On() || s.Off != "" {
		t.Fatalf("segment unset: %+v", s)
	}
	if r := (DunConfig{Facets: map[string]Facet{FacetIdentity: {Model: "x"}}}).Resolve(FacetIdentity, nil, nil); r.On() || r.Off != "no provider is logged in" {
		t.Fatalf("logged out: %+v", r)
	}
	if r := c.Resolve(FacetMain, Overrides{FacetMain: {Provider: "gone"}}, nil); r.On() || !strings.Contains(r.Off, "no saved provider gone") {
		t.Fatalf("unsaved provider: %+v", r)
	}
}

// Loading never rewrites the file; Use writes facets.main (and the old
// field, for an older binary).
func TestFacetsLoadIsReadOnlyAndUseWritesFacets(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := []byte(`{"url":"http://gw:4000","model":"old-chat","vision_model":"vl"}` + "\n")
	if err := os.WriteFile(ConfigPath(), old, 0o600); err != nil {
		t.Fatal(err)
	}
	c := LoadConfig()
	_ = c.ResolveAll(nil, nil)
	if b, _ := os.ReadFile(ConfigPath()); string(b) != string(old) {
		t.Fatalf("load rewrote the file: %s", b)
	}
	p, _ := c.Active()
	c.Use(p, "new-chat")
	if f := c.Facets[FacetMain]; f.Model != "new-chat" || f.Provider != "gw" || c.Model != "new-chat" {
		t.Fatalf("use: %+v %q", c.Facets, c.Model)
	}
	if err := SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOp(ProviderOp{Op: "use", Name: "gw", Model: "e5", Facet: FacetEmbed}); err != nil {
		t.Fatal(err)
	}
	got := LoadConfig()
	if got.Facets[FacetEmbed].Model != "e5" || got.Facets[FacetMain].Model != "new-chat" || got.Model != "new-chat" {
		t.Fatalf("saved: %+v", got)
	}
	if _, err := ApplyProviderOp(ProviderOp{Op: "use", Name: "gw", Model: "x", Facet: "typo"}); err == nil {
		t.Fatal("a facet that is no role was accepted")
	}
	if _, err := ApplyProviderOp(ProviderOp{Op: "rename", Name: "gw", NewName: "gw2"}); err != nil {
		t.Fatal(err)
	}
	if f := LoadConfig().Facets; f[FacetMain].Provider != "gw2" || f[FacetEmbed].Provider != "gw2" {
		t.Fatalf("rename keeps the facets on it: %+v", f)
	}
}

func TestResolvedLineAndParsePick(t *testing.T) {
	c := facetCfg()
	lines := []string{}
	for _, r := range c.ResolveAll(nil, nil) {
		lines = append(lines, r.Line())
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{"main      old-chat on gw · config, old field", "child     same as main · default",
		"vision    old-vl on far · config, old field", "embed     local-nomic-embed-text on gw · default", "segment   off · default"} {
		if !strings.Contains(all, want) {
			t.Fatalf("lines\n%s\nmissing %q", all, want)
		}
	}
	if strings.Contains(all, "k1") || strings.Contains(all, "k2") {
		t.Fatal("a key in the lines")
	}
	for in, want := range map[string][2]string{"far/m1": {"far", "m1"}, "anthropic/claude-x": {"", "anthropic/claude-x"}, "m2": {"", "m2"}} {
		if p, m := c.ParsePick(in); p != want[0] || m != want[1] {
			t.Errorf("ParsePick(%q) = %q %q", in, p, m)
		}
	}
}
