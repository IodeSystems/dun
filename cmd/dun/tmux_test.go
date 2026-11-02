package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestTmuxCmd(t *testing.T) {
	q := func(formats map[string]string) tmuxQuery {
		return func(args ...string) (string, error) { return formats[args[len(args)-1]], nil }
	}
	var b bytes.Buffer
	tmuxCmd(&b, true, false, q(map[string]string{"window-status-format": "#I #W", "window-status-current-format": "#[bold]#I #W"}))
	out := b.String()
	for _, want := range []string{
		"set -g window-status-format '#{?@dun_icon,#{@dun_icon} ,}#I #W'",
		"set -g window-status-current-format '#{?@dun_icon,#{@dun_icon} ,}#[bold]#I #W'",
		"does not show the icon yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "allow-passthrough") {
		t.Error("passthrough line outside iTerm2")
	}
	b.Reset()
	both := tmuxIconPrefix + tmuxDefaultFormat
	tmuxCmd(&b, true, true, q(map[string]string{"window-status-format": both, "window-status-current-format": both}))
	if !strings.Contains(b.String(), "shows the icon already") || !strings.Contains(b.String(), "set -g allow-passthrough on") {
		t.Errorf("configured, in iTerm2:\n%s", b.String())
	}
	b.Reset()
	tmuxCmd(&b, false, false, func(...string) (string, error) { t.Fatal("queried tmux outside tmux"); return "", nil })
	if !strings.Contains(b.String(), "not inside tmux") || !strings.Contains(b.String(), "'"+tmuxIconPrefix+tmuxDefaultFormat+"'") {
		t.Errorf("outside tmux:\n%s", b.String())
	}
}

func TestWebEnvDropsTerminal(t *testing.T) {
	got := webEnv([]string{"PATH=/bin", "TMUX=/tmp/s,1,0", "TMUX_PANE=%1", "TERM_PROGRAM=iTerm.app", "LC_TERMINAL=iTerm2", "HOME=/h"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Fatalf("%v", got)
	}
}
