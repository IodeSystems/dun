package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/iodesystems/dun"
)

// Key maps (USER 2026-10-28: "vim crew vs emacs crew vs vscode crew").
//
// The key handlers keep matching the keys they always matched. A key map is a
// translation in front of them: the key pressed is looked up, and if it is
// bound to an action, the action's own key (the first of its defaults — the
// one the code matches) is delivered instead. A default key whose action was
// bound elsewhere is dropped, so moving an action moves it rather than adding
// a second key. One table, so a key bound twice in one scope is an error at
// load, naming both actions (ctrl+g was nearly the "go to the bottom" key
// while it already interrupted a turn).
//
// Scopes: "global" keys work whatever has the keys (the interrupt); "main" is
// the conversation and the composer under it. Pagers, pickers, forms and the
// panes keep fixed keys for now, and so do keys that type text ("/", "y") —
// rebinding one in "main" would stop it being typed in the composer.
//
// Not here yet: a modal (vim) composer. Normal and insert modes are a
// different composer, not a different table.

// keyAction is one thing a key does.
type keyAction struct {
	name, scope, desc string
	keys              []string // defaults; keys[0] is the one the handlers match
}

// keyActions is every rebindable action, in the order /keys lists them.
var keyActions = []keyAction{
	{"turn.interrupt", "global", "interrupt a running turn", []string{interruptKey}},

	{"send", "main", "send the message", []string{"enter"}},
	{"newline", "main", "a new line in the message (shift+enter too, where the terminal reports it)", []string{"alt+enter", "ctrl+j"}},
	{"tab.insert", "main", "type a tab", []string{"ctrl+tab"}},
	{"focus.next", "main", "the next zone", []string{"tab"}},
	{"focus.prev", "main", "the previous zone", []string{"shift+tab"}},

	{"scroll.bottom", "main", "the newest output, following again", []string{"ctrl+end"}},
	{"scroll.turn", "main", "the top of your last message; again, the one before", []string{"ctrl+home"}},
	{"page.up", "main", "a page up", []string{"pgup"}},
	{"page.down", "main", "a page down", []string{"pgdown"}},
	{"history.prev", "main", "the previous message you sent", []string{"ctrl+up"}},
	{"history.next", "main", "the next message you sent", []string{"ctrl+down"}},

	{"move.up", "main", "up: a line, a block, or history at the input's top", []string{"up"}},
	{"move.down", "main", "down", []string{"down"}},
	{"move.left", "main", "left", []string{"left", "ctrl+b"}},
	{"move.right", "main", "right", []string{"right", "ctrl+f"}},
	{"line.start", "main", "the start of the line", []string{"ctrl+a", "home"}},
	{"line.end", "main", "the end of the line", []string{"ctrl+e", "end"}},
	{"word.left", "main", "a word left", []string{"alt+left", "ctrl+left", "alt+b"}},
	{"word.right", "main", "a word right", []string{"alt+right", "ctrl+right", "alt+f"}},
	{"delete.word_back", "main", "delete the word before the cursor", []string{"ctrl+w", "alt+backspace", "ctrl+backspace"}},
	{"delete.word_forward", "main", "delete the word after the cursor", []string{"alt+d"}},
	{"delete.to_end", "main", "delete to the end of the line", []string{"ctrl+k"}},
	{"delete.to_start", "main", "delete to the start of the line", []string{"ctrl+u"}},
	{"delete.char", "main", "delete the character under the cursor", []string{"ctrl+d", "delete"}},
	{"paste.text", "main", "paste the clipboard's text", []string{"ctrl+p"}},
	{"paste.image", "main", "paste the clipboard's image", []string{"ctrl+v"}},
	{"paste.expand", "main", "open a paste token for editing", []string{"ctrl+o"}},
}

// keyPresets are named sets of changes to the defaults. emacs is the defaults
// (readline motions, ctrl+g cancels); vscode moves the motions to the keys an
// editor uses and leaves the rest.
var keyPresets = map[string]map[string][]string{
	"emacs": {},
	"vscode": {
		"newline":          {"shift+enter", "alt+enter", "ctrl+j"},
		"line.start":       {"home"},
		"line.end":         {"end"},
		"word.left":        {"ctrl+left", "alt+left"},
		"word.right":       {"ctrl+right", "alt+right"},
		"delete.word_back": {"ctrl+backspace", "alt+backspace"},
		"move.left":        {"left"},
		"move.right":       {"right"},
		"delete.char":      {"delete"},
		"delete.to_start":  {},
	},
}

// keyMap is the effective binding: action → keys, and per scope key → action.
type keyMap struct {
	preset string
	bound  map[string][]string          // action → its keys now
	from   map[string]string            // action → "default", "preset" or "keys.json"
	byKey  map[string]map[string]string // scope → key → action
	dflt   map[string]map[string]string // scope → default key → action
}

// keysFile is where a person's bindings live.
func keysFile() string { return filepath.Join(dun.DunHome(), "keys.json") }

// keysConfig is keys.json: a preset, and actions bound to keys of your own.
// An action given [] has no key.
type keysConfig struct {
	Preset string              `json:"preset,omitempty"`
	Bind   map[string][]string `json:"bind,omitempty"`
}

// defaultKeyMap is the map with no keys.json.
func defaultKeyMap() keyMap {
	km, _ := buildKeyMap(keysConfig{})
	return km
}

// loadKeyMap reads keys.json. A missing file is the defaults; a broken one is
// the defaults AND the error, so a typo never leaves dun with no keys.
func loadKeyMap() (keyMap, error) {
	b, err := os.ReadFile(keysFile())
	if errors.Is(err, os.ErrNotExist) {
		return defaultKeyMap(), nil
	}
	if err != nil {
		return defaultKeyMap(), err
	}
	var cfg keysConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultKeyMap(), fmt.Errorf("%s: %w", keysFile(), err)
	}
	km, err := buildKeyMap(cfg)
	if err != nil {
		return defaultKeyMap(), fmt.Errorf("%s: %w", keysFile(), err)
	}
	return km, nil
}

func actionByName(name string) (keyAction, bool) {
	for _, a := range keyActions {
		if a.name == name {
			return a, true
		}
	}
	return keyAction{}, false
}

// buildKeyMap applies a preset and then the person's own bindings to the
// defaults, and checks the result.
func buildKeyMap(cfg keysConfig) (keyMap, error) {
	km := keyMap{preset: cfg.Preset, bound: map[string][]string{}, from: map[string]string{},
		byKey: map[string]map[string]string{}, dflt: map[string]map[string]string{}}
	if km.preset == "" {
		km.preset = "emacs"
	}
	preset, ok := keyPresets[km.preset]
	if !ok {
		return km, fmt.Errorf("no preset %q (there are %s)", cfg.Preset, strings.Join(presetNames(), ", "))
	}
	for _, a := range keyActions {
		km.bound[a.name], km.from[a.name] = a.keys, "default"
		if km.dflt[a.scope] == nil {
			km.dflt[a.scope] = map[string]string{}
		}
		for _, k := range a.keys {
			km.dflt[a.scope][k] = a.name
		}
	}
	apply := func(src string, set map[string][]string) error {
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			a, ok := actionByName(name)
			if !ok {
				return fmt.Errorf("no action %q (/keys lists them)", name)
			}
			keys := make([]string, 0, len(set[name]))
			for _, k := range set[name] {
				norm, err := normKey(k)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if a.scope == "main" && typesText(norm) {
					return fmt.Errorf("%s: %q types text, so the composer could no longer type it — use a key with ctrl or alt", name, k)
				}
				keys = append(keys, norm)
			}
			km.bound[name], km.from[name] = keys, src
		}
		return nil
	}
	if err := apply("preset", preset); err != nil {
		return km, err
	}
	if err := apply("keys.json", cfg.Bind); err != nil {
		return km, err
	}
	// One key, one action, per scope. A global key counts in every scope.
	for _, a := range keyActions {
		if km.byKey[a.scope] == nil {
			km.byKey[a.scope] = map[string]string{}
		}
		for _, k := range km.bound[a.name] {
			if other, ok := km.byKey[a.scope][k]; ok && other != a.name {
				return km, fmt.Errorf("%s is bound to both %s and %s — give one of them another key", k, other, a.name)
			}
			if a.scope != "global" {
				if other, ok := km.byKey["global"][k]; ok {
					return km, fmt.Errorf("%s is bound to both %s and %s — give one of them another key", k, other, a.name)
				}
			}
			km.byKey[a.scope][k] = a.name
		}
	}
	return km, nil
}

func presetNames() []string {
	out := make([]string, 0, len(keyPresets))
	for n := range keyPresets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// translate turns the key pressed in "main" into the key the handlers match,
// or nil for a default key whose action was given other keys. Elsewhere it
// changes nothing: a pager's own keys are not this table's, and a global key
// is asked for by name (is), never translated — ctrl+g means "global switch"
// in the MCP lists, so a rebound interrupt delivered as ctrl+g would flip one.
func (km keyMap) translate(scope string, msg tea.Msg) tea.Msg {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || km.byKey == nil || scope != "main" {
		return msg
	}
	s := k.String()
	if name, ok := km.byKey["main"][s]; ok {
		if km.dflt["main"][s] == name {
			return msg // one of its own defaults: the handlers match it as is
		}
		a, _ := actionByName(name)
		out, _ := parseKey(a.keys[0])
		return out
	}
	if _, ok := km.dflt["main"][s]; ok {
		return nil // its action was given other keys
	}
	return msg
}

// is reports whether key is bound to action now.
func (km keyMap) is(action, key string) bool {
	if km.bound == nil {
		return key != "" && slices.Contains(defaultKeyMap().bound[action], key)
	}
	return slices.Contains(km.bound[action], key)
}

// label is how a hint names an action's key: its first, or "" if it has none.
func (km keyMap) label(action string) string {
	if ks := km.bound[action]; len(ks) > 0 {
		return ks[0]
	}
	return ""
}

// keyScope is "main" when the conversation and composer have the keys, else
// the name of what does (it matters only that it is not "main").
func (m tuiModel) keyScope() string {
	switch {
	case m.confirm.q != "", m.inspecting, m.jobViewing, m.activeScreen() != nil, m.paging,
		m.asking, m.picking, m.mcpForm, m.rag != nil, m.modelPicking, m.searching:
		return "modal"
	}
	return "main"
}

// keysReport is /keys: every action, its keys, and where they came from.
func (km keyMap) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "keys · preset %s (%s) · yours in %s\n", km.preset, strings.Join(presetNames(), ", "), keysFile())
	w := 0
	for _, a := range keyActions {
		w = max(w, len(a.name))
	}
	for _, a := range keyActions {
		keys := strings.Join(km.bound[a.name], ", ")
		if keys == "" {
			keys = "(none)"
		}
		src := ""
		if f := km.from[a.name]; f != "default" {
			src = "  ← " + f
		}
		fmt.Fprintf(&b, "  %-*s  %-28s %s%s\n", w, a.name, keys, a.desc, src)
	}
	b.WriteString("fixed: ctrl+c, esc, / and y in the conversation, and the keys inside pagers, pickers and forms\n")
	b.WriteString(`keys.json: {"preset": "vscode", "bind": {"scroll.bottom": ["ctrl+g"], "turn.interrupt": ["ctrl+x"]}} · /keys reload`)
	return b.String()
}

// typesText reports whether a key, pressed in the composer, would type
// something: a printable character with no ctrl or alt, or space.
func typesText(key string) bool {
	k, err := parseKey(key)
	if err != nil {
		return false
	}
	return k.Text != "" && !k.Mod.Contains(tea.ModCtrl) && !k.Mod.Contains(tea.ModAlt)
}

// normKey is a key as Bubble Tea names it, from however a person wrote it
// ("Ctrl+G", "alt+ctrl+x").
func normKey(s string) (string, error) {
	k, err := parseKey(s)
	if err != nil {
		return "", err
	}
	return k.String(), nil
}

var namedKeys = map[string]rune{
	"enter": tea.KeyEnter, "tab": tea.KeyTab, "esc": tea.KeyEscape, "escape": tea.KeyEscape,
	"space": tea.KeySpace, "backspace": tea.KeyBackspace, "delete": tea.KeyDelete, "insert": tea.KeyInsert,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
	"f1": tea.KeyF1, "f2": tea.KeyF2, "f3": tea.KeyF3, "f4": tea.KeyF4, "f5": tea.KeyF5, "f6": tea.KeyF6,
	"f7": tea.KeyF7, "f8": tea.KeyF8, "f9": tea.KeyF9, "f10": tea.KeyF10, "f11": tea.KeyF11, "f12": tea.KeyF12,
}

// parseKey is the key press a name stands for: modifiers (ctrl, alt, shift)
// joined by "+", then a named key or one character.
func parseKey(s string) (tea.KeyPressMsg, error) {
	parts := strings.Split(strings.TrimSpace(s), "+")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return tea.KeyPressMsg{}, fmt.Errorf("%q is not a key", s)
	}
	var k tea.KeyPressMsg
	for _, p := range parts[:len(parts)-1] {
		switch strings.ToLower(p) {
		case "ctrl":
			k.Mod |= tea.ModCtrl
		case "alt":
			k.Mod |= tea.ModAlt
		case "shift":
			k.Mod |= tea.ModShift
		default:
			return tea.KeyPressMsg{}, fmt.Errorf("%q: no modifier %q (ctrl, alt, shift)", s, p)
		}
	}
	last := parts[len(parts)-1]
	if code, ok := namedKeys[strings.ToLower(last)]; ok {
		k.Code = code
		if code == tea.KeySpace && k.Mod == 0 {
			k.Text = " "
		}
		return k, nil
	}
	if r, n := utf8.DecodeRuneInString(last); n == len(last) && r != utf8.RuneError {
		k.Code = r
		if k.Mod&^tea.ModShift == 0 {
			k.Text = string(r)
		}
		return k, nil
	}
	return tea.KeyPressMsg{}, fmt.Errorf("%q: no key %q", s, last)
}

// composeHint is the ready status line's key hint for the composer.
func (m tuiModel) composeHint() string {
	var parts []string
	if k := m.keys.label("send"); k != "" {
		parts = append(parts, k+" send")
	}
	if k := m.keys.label("newline"); k != "" {
		parts = append(parts, k+" newline")
	}
	parts = append(parts, "↑ history")
	return strings.Join(parts, " · ") + " · "
}

// keysSlash is /keys: the map, /keys reload (re-read keys.json), and
// /keys preset <name> (saved to keys.json, your own bindings kept).
func keysSlash(m *tuiModel, args []string) tea.Cmd {
	switch {
	case len(args) == 0:
		m.append(stDim.Render(m.keys.report()))
	case args[0] == "reload":
		km, err := loadKeyMap()
		m.keys = km
		if err != nil {
			m.append(warnLine("keys: " + err.Error() + " — using the defaults"))
			return nil
		}
		m.append(stDim.Render("keys: reloaded · preset " + km.preset))
	case args[0] == "preset" && len(args) == 2:
		if err := setKeyPreset(args[1]); err != nil {
			m.append(stErr.Render("keys: " + err.Error()))
			return nil
		}
		km, err := loadKeyMap()
		m.keys = km
		if err != nil {
			m.append(warnLine("keys: " + err.Error() + " — using the defaults"))
			return nil
		}
		m.append(stDim.Render("keys: preset " + km.preset + ", saved to " + keysFile()))
	default:
		m.append(stErr.Render("keys: want /keys, /keys reload or /keys preset <" + strings.Join(presetNames(), "|") + ">"))
	}
	m.refresh()
	return nil
}

// setKeyPreset writes the preset into keys.json, keeping whatever else is
// there, and refuses a preset that would not load with your own bindings.
func setKeyPreset(name string) error {
	if _, ok := keyPresets[name]; !ok {
		return fmt.Errorf("no preset %q (there are %s)", name, strings.Join(presetNames(), ", "))
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(keysFile()); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("%s: %w — fix it first", keysFile(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var cfg keysConfig
	if b, ok := raw["bind"]; ok {
		if err := json.Unmarshal(b, &cfg.Bind); err != nil {
			return fmt.Errorf("%s: bind: %w", keysFile(), err)
		}
	}
	cfg.Preset = name
	if _, err := buildKeyMap(cfg); err != nil {
		return err
	}
	raw["preset"], _ = json.Marshal(name)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keysFile()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(keysFile(), append(out, '\n'), 0o644)
}
