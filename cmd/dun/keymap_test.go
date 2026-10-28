package main

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// writeKeys points DUN_HOME at a fresh directory holding this keys.json.
func writeKeys(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	if body != "" {
		if err := os.WriteFile(keysFile(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func press(t *testing.T, s string) tea.KeyPressMsg {
	t.Helper()
	k, err := parseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Every default is a key Bubble Tea names the same way, or the handlers
// would never see it; and every preset loads on its own.
func TestKeyMap_DefaultsAndPresetsLoad(t *testing.T) {
	for _, a := range keyActions {
		for _, k := range a.keys {
			if p, err := parseKey(k); err != nil || p.String() != k {
				t.Errorf("%s: %q parses to %q (%v)", a.name, k, p.String(), err)
			}
		}
	}
	for _, name := range presetNames() {
		if _, err := buildKeyMap(keysConfig{Preset: name}); err != nil {
			t.Errorf("preset %s: %v", name, err)
		}
	}
}

// Every action can be moved to a key of its own, and that key then delivers
// the key its handler matches; every default of an unmoved action passes
// through as is.
func TestKeyMap_EveryActionMovesAndEveryDefaultPasses(t *testing.T) {
	km := defaultKeyMap()
	for _, a := range keyActions {
		if a.scope != "main" {
			continue
		}
		for _, k := range a.keys {
			if got := km.translate("main", press(t, k)); got == nil || got.(tea.KeyPressMsg).String() != k {
				t.Errorf("%s: default %s did not pass through: %v", a.name, k, got)
			}
		}
		moved, err := buildKeyMap(keysConfig{Bind: map[string][]string{a.name: {"f12"}}})
		if err != nil {
			t.Fatalf("%s to f12: %v", a.name, err)
		}
		got := moved.translate("main", press(t, "f12"))
		if got == nil || got.(tea.KeyPressMsg).String() != a.keys[0] {
			t.Errorf("%s on f12 delivered %v, want %s", a.name, got, a.keys[0])
		}
		for _, k := range a.keys {
			if moved.translate("main", press(t, k)) != nil {
				t.Errorf("%s moved to f12, but its default %s still works", a.name, k)
			}
		}
	}
}

// One key, one action: the error names both, and the defaults stay in use.
func TestKeyMap_ConflictsAndTypingKeysAreRefused(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"bind":{"scroll.bottom":["ctrl+g"]}}`, "ctrl+g is bound to both turn.interrupt and scroll.bottom"},
		{`{"bind":{"scroll.bottom":["ctrl+a"]}}`, "ctrl+a is bound to both"},
		{`{"bind":{"scroll.bottom":["G"]}}`, "types text"},
		{`{"bind":{"scroll.bottom":["space"]}}`, "types text"},
		{`{"bind":{"scroll.botom":["f5"]}}`, `no action "scroll.botom"`},
		{`{"preset":"vim"}`, `no preset "vim"`},
		{`{"bind":{"scroll.bottom":["hyper+x"]}}`, "no modifier"},
		{`{"bind":`, "keys.json"},
	} {
		writeKeys(t, tc.body)
		km, err := loadKeyMap()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want it to say %q", tc.body, err, tc.want)
		}
		if km.label("scroll.bottom") != "ctrl+end" {
			t.Errorf("%s: a broken keys.json must leave the defaults, got %q", tc.body, km.label("scroll.bottom"))
		}
	}
	// Moving the interrupt first frees ctrl+g.
	writeKeys(t, `{"bind":{"scroll.bottom":["ctrl+g"],"turn.interrupt":["ctrl+x"]}}`)
	if _, err := loadKeyMap(); err != nil {
		t.Fatal(err)
	}
}

// The ctrl+g a person wanted: going to the bottom, with the interrupt moved
// to ctrl+x. While a turn runs, ctrl+g scrolls and does not interrupt; ctrl+x
// interrupts. The cue and the busy line name the keys as bound.
func TestKeyMap_CtrlGToTheBottomInterruptOnCtrlX(t *testing.T) {
	writeKeys(t, `{"bind":{"scroll.bottom":["ctrl+g"],"turn.interrupt":["ctrl+x"]}}`)
	v, rec := busyVtui(t)
	for i := 0; i < 30; i++ {
		v.event(map[string]any{"type": "token", "text": "a line of the reply\n\n"})
	}
	v.m.refresh()
	v.setScrollPin(false)
	v.setYOffset(0)
	v.m.refresh()
	if !strings.Contains(v.view(), "↓ newest · ctrl+g") {
		t.Fatalf("the cue should name ctrl+g:\n%s", v.view())
	}
	if !strings.Contains(v.view(), "ctrl+c or ctrl+x interrupt") {
		t.Errorf("the busy line should name ctrl+x:\n%s", v.view())
	}
	v.m = key(v.m, kCtrlG)
	if sentInterrupt(rec) {
		t.Fatal("ctrl+g interrupted; it is the bottom key now")
	}
	if !v.m.scrollPinned || v.m.vp.YOffset != v.m.vp.maxYOffset() {
		t.Fatalf("ctrl+g did not go to the bottom: offset %d of %d", v.m.vp.YOffset, v.m.vp.maxYOffset())
	}
	v.m = key(v.m, press(t, "ctrl+x"))
	if !sentInterrupt(rec) {
		t.Fatal("ctrl+x did not interrupt")
	}
}

// A composer key moved away stops working; its new key does the job.
func TestKeyMap_MovedComposerKey(t *testing.T) {
	writeKeys(t, `{"bind":{"delete.to_end":["alt+k"]}}`)
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.m = typeStr(v.m, "keep this")
	v.m = key(v.m, press(t, "ctrl+a"))
	v.m = key(v.m, kCtrlK)
	if got := v.m.input.Value(); got != "keep this" {
		t.Fatalf("ctrl+k still deleted: %q", got)
	}
	v.m = key(v.m, press(t, "alt+k"))
	if got := v.m.input.Value(); got != "" {
		t.Fatalf("alt+k should delete to the end: %q", got)
	}
}

// /keys lists every action; /keys preset saves the preset beside your own
// bindings and applies it now.
func TestKeyMap_SlashKeys(t *testing.T) {
	writeKeys(t, `{"bind":{"scroll.bottom":["ctrl+g"],"turn.interrupt":["ctrl+x"]}}`)
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	keysSlash(&v.m, nil)
	out := v.convo()
	for _, want := range []string{"scroll.bottom", "ctrl+g", "← keys.json", "line.start", "preset emacs"} {
		if !strings.Contains(out, want) {
			t.Errorf("/keys lacks %q:\n%s", want, out)
		}
	}
	keysSlash(&v.m, []string{"preset", "vscode"})
	if v.m.keys.preset != "vscode" || v.m.keys.label("scroll.bottom") != "ctrl+g" {
		t.Fatalf("preset not applied with the bindings kept: %s, %q", v.m.keys.preset, v.m.keys.label("scroll.bottom"))
	}
	b, _ := os.ReadFile(keysFile())
	if !strings.Contains(string(b), `"preset": "vscode"`) || !strings.Contains(string(b), "ctrl+x") {
		t.Fatalf("keys.json after /keys preset:\n%s", b)
	}
	// vscode moves line.start to home: ctrl+a is free, and does nothing.
	v.m = typeStr(v.m, "ab")
	v.m = key(v.m, press(t, "ctrl+a"))
	v.m = typeStr(v.m, "c")
	if got := v.m.input.Value(); got != "abc" {
		t.Errorf("under vscode ctrl+a moved the cursor: %q", got)
	}
}

// A broken keys.json is said once, at ready, and the defaults work.
func TestKeyMap_BrokenFileIsSaidOnce(t *testing.T) {
	writeKeys(t, `{"bind":{"scroll.bottom":["ctrl+a"]}}`)
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	if n := strings.Count(v.convo(), "keys: "); n != 1 {
		t.Fatalf("the keys.json error was said %d times:\n%s", n, v.convo())
	}
}
