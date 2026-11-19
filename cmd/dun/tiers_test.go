package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// A tier the engine cannot serve, and a window-sized side call put on a
// tier, are said at engine start; the tiers reach the settings event as the
// engine resolved them.
func TestStartTiers_Warnings(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	fc := dunconfig.DunConfig{
		Providers: []dunconfig.ProviderConfig{{Name: "gw", URL: "http://gw"}},
		Tiers:     map[string]dunconfig.Tier{dunconfig.TierFast: {Provider: "gone", Model: "small"}},
		SideCalls: map[string]string{"rescue": "fast"},
	}
	var w bytes.Buffer
	set, sides := startTiers(fc, t.TempDir(), false, &w)
	out := w.String()
	for _, want := range []string{"dun: tier fast is unavailable (no saved provider gone)", `dun: side_calls.rescue "fast": rescue sends up to the session's window`} {
		if !strings.Contains(out, want) {
			t.Errorf("start lacks %q:\n%s", want, out)
		}
	}
	if sides["rescue"] != "fast" || sides["fold"] != dunconfig.TierSession {
		t.Errorf("sides %v", sides)
	}
	tp := tiersProto(set)
	if len(tp) != 3 || tp[0].Model != "small" || tp[0].Off == "" {
		t.Errorf("settings tiers %+v", tp)
	}
}
