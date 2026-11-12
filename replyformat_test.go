package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/bubbleteandmd/prompt"
)

// The answer-formatting section is in a root, interactive session by default;
// DUN_FORMAT_PROMPT=0 (NoFormatPrompt), --non-interactive (the Harbor
// adapter's mode) and a sub-agent leave it out.
func TestFormatPrompt_Modes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	section := prompt.Format(prompt.Options{})
	if section == "" {
		t.Fatal("empty section")
	}
	for name, c := range map[string]struct {
		cfg  Config
		want bool
	}{
		"default":         {Config{}, true},
		"switch off":      {Config{NoFormatPrompt: true}, false},
		"non-interactive": {Config{NonInteractive: true}, false},
	} {
		c.cfg.Workspace, c.cfg.Servers = ws, []Server{}
		h, err := Start(context.Background(), c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(h.Session.System, section); got != c.want {
			t.Errorf("%s: section present=%v, want %v", name, got, c.want)
		}
		if name == "default" {
			child, err := Start(context.Background(), func() Config {
				cc := childConfig(h, 1, childRun{}, isolation{})
				cc.SessionFile = ""
				return cc
			}())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(child.Session.System, section) {
				t.Error("sub-agent has the section")
			}
			child.Close()
		}
		h.Close()
	}
}
