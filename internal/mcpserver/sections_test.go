package mcpserver

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/project"
)

// A repo's dun.json sets a tier's use text and default_tier; a model,
// provider or effort there is named and has no effect. $DUN_HOME/mcp.json
// is not a tier layer.
func TestLoadTiers_RepoNarrowing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	dir := t.TempDir()
	writeJSON(t, home, "mcp.json", `{"default_tier":"fast"}`)
	writeJSON(t, dir, project.ProjectServersFile,
		`{"default_tier":"think","tiers":{"fast":{"use":"repo fast","model":"pricey","provider":"far"},"work":{"effort":"high"}}}`)
	got := LoadTiers(dir)
	if got.Default != "think" {
		t.Errorf("default %q, want the repo's think", got.Default)
	}
	if got.Use["fast"] != "repo fast" || len(got.Use) != 1 {
		t.Errorf("use %v", got.Use)
	}
	ign := strings.Join(got.Ignored, "; ")
	for _, want := range []string{"tiers.fast.provider", "tiers.fast.model", "tiers.work.effort"} {
		if !strings.Contains(ign, want) {
			t.Errorf("ignored %q lacks %s", ign, want)
		}
	}
	if len(got.Ignored) != 3 {
		t.Errorf("ignored %v", got.Ignored)
	}
}
