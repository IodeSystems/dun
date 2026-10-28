package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestItermDetect(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, true},
		{map[string]string{"LC_TERMINAL": "iTerm2"}, true},
		{map[string]string{"TERM_PROGRAM": "tmux", "LC_TERMINAL": "iTerm2"}, true},
		{map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, false},
		{map[string]string{"TERM_PROGRAM": "WezTerm"}, false},
		{map[string]string{}, false},
	}
	for _, c := range cases {
		if got := itermDetect(tmuxEnv(c.env)); got != c.want {
			t.Errorf("%v: %v, want %v", c.env, got, c.want)
		}
	}
}

// Off by default, even in iTerm2; on with the flag, in iTerm2 only.
func TestItermTabOptIn(t *testing.T) {
	iterm := tmuxEnv(map[string]string{"TERM_PROGRAM": "iTerm.app"})
	if newItermTab(false, iterm, "/w/proj") != nil {
		t.Fatal("on without --exp-iterm-tabcontrol")
	}
	if newItermTab(true, tmuxEnv(map[string]string{"TERM_PROGRAM": "Apple_Terminal"}), "/w/proj") != nil {
		t.Fatal("on in another terminal")
	}
	tab := newItermTab(true, iterm, "/w/proj")
	if tab == nil || tab.title != "dun · proj" || tab.wrap {
		t.Fatalf("with the flag in iTerm2: %+v", tab)
	}
	if w := newItermTab(true, tmuxEnv(map[string]string{"LC_TERMINAL": "iTerm2", "TMUX": "x"}), "/w/proj"); w == nil || !w.wrap {
		t.Fatalf("in tmux in iTerm2: %+v", w)
	}
}

// The flag is this client's: the engine's argv never carries it.
func TestItermFlagNotForwarded(t *testing.T) {
	o := tuiOpts{workspace: "/w", model: "m", itermTabs: true, suggest: true}
	for _, mode := range []string{"-p", "--tui"} {
		for _, a := range procArgs(o, mode) {
			if strings.Contains(a, "iterm") {
				t.Fatalf("%s argv carries %q: %v", mode, a, procArgs(o, mode))
			}
		}
	}
}

func TestItermEscapes(t *testing.T) {
	tab := &itermTab{title: "dun · proj"}
	got := tab.show(attnWorking, false)
	want := "\x1b[22;1t" + "\x1b]1;⚙ dun · proj\a" +
		"\x1b]6;1;bg;red;brightness;200\a\x1b]6;1;bg;green;brightness;150\a\x1b]6;1;bg;blue;brightness;60\a"
	if got != want {
		t.Fatalf("working:\n%q\nwant\n%q", got, want)
	}
	// Pushed once only.
	got = tab.show(attnQuestion, true)
	want = "\x1b]1;❓ dun · proj\a" +
		"\x1b]6;1;bg;red;brightness;220\a\x1b]6;1;bg;green;brightness;110\a\x1b]6;1;bg;blue;brightness;160\a" +
		"\x1b]1337;RequestAttention=yes\a"
	if got != want {
		t.Fatalf("question:\n%q\nwant\n%q", got, want)
	}
	if got := tab.show(attnQuestion, false); strings.Contains(got, "RequestAttention") {
		t.Fatal("asked for attention with the bell off")
	}
	got = tab.show(attnDone, false)
	want = "\x1b]1;✓ dun · proj\a" +
		"\x1b]6;1;bg;red;brightness;90\a\x1b]6;1;bg;green;brightness;170\a\x1b]6;1;bg;blue;brightness;100\a"
	if got != want {
		t.Fatalf("done:\n%q\nwant\n%q", got, want)
	}
	got = tab.show(attnIdle, false)
	want = "\x1b]1;dun · proj\a\x1b]6;1;bg;*;default\a"
	if got != want {
		t.Fatalf("idle:\n%q\nwant\n%q", got, want)
	}
	got = tab.restore()
	want = "\x1b]6;1;bg;*;default\a\x1b[23;1t"
	if got != want {
		t.Fatalf("restore:\n%q\nwant\n%q", got, want)
	}
	if tab.restore() != "" {
		t.Fatal("restored twice")
	}
	if (&itermTab{}).restore() != "" {
		t.Fatal("restored a title it never pushed")
	}
}

func TestItermTmuxWrap(t *testing.T) {
	if got, want := tmuxWrap("\x1b]1;t\a"), "\x1bPtmux;\x1b\x1b]1;t\a\x1b\\"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	tab := &itermTab{title: "dun · p", wrap: true, pushed: true}
	got := tab.show(attnIdle, false)
	want := "\x1bPtmux;\x1b\x1b]1;dun · p\a\x1b\\" + "\x1bPtmux;\x1b\x1b]6;1;bg;*;default\a\x1b\\"
	if got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

// Through syncAttn: the bell first, then the tab; nothing for a nil tab.
func TestItermThroughSync(t *testing.T) {
	t.Setenv("DUN_BELL", "")
	tab := &itermTab{title: "dun · p", pushed: true}
	prev := tuiModel{busy: true, attnState: attnWorking, iterm: tab}
	ask := prev
	ask.asking = true
	_, cmd := syncAttn(prev, ask, nil)
	got := rawOf(t, cmd)
	if !strings.HasPrefix(got, "\a\x1b]1;❓ dun · p\a") || !strings.HasSuffix(got, "RequestAttention=yes\a") {
		t.Fatalf("%q", got)
	}
	// Other terminals: the bell and nothing else.
	prev.iterm, ask.iterm = nil, nil
	if _, cmd := syncAttn(prev, ask, nil); rawOf(t, cmd) != "\a" {
		t.Fatal("wrote more than the bell outside iTerm2")
	}
	// No state change, no write.
	if _, cmd := syncAttn(ask, func() tuiModel { a := ask; a.attnState = attnQuestion; return a }(), tea.KeyPressMsg{}); cmd != nil {
		t.Fatal("wrote without a change")
	}
}
