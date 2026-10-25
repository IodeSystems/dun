package dun

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Scoped switches: a kind of thing the person turns on and off at the three
// scopes /mcp has (mcpscope.go), with a mode for the kind as a whole. Skills
// are the first kind (skills.go); context files are next. /mcp keeps its own
// storage, in the server specs (USER 2026-10-24: /mcp, /skill and
// /context-file follow one pattern — make it one).
//
//	global   $DUN_HOME/<kind>.json
//	project  the project's state dir, <kind>.json
//	session  the session's .<kind>.json sidecar, beside its .jsonl
//
// Each holds {"mode": "<mode>", "on": {"<name or glob>": true|false}}. An
// item's switch is the session's if one matches it, else the project's, else
// the global one, else on; within a scope an exact name beats a glob, and a
// longer glob a shorter one. The mode resolves the same way, to the kind's
// default when no scope sets it.

// SwitchFile is one scope's file of one kind.
type SwitchFile struct {
	Mode string          `json:"mode,omitempty"`
	On   map[string]bool `json:"on,omitempty"`
}

// SwitchItem is one thing a kind switches, as its kind describes it.
type SwitchItem struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`
	Detail      string `json:"detail,omitempty"` // more, shown when the item is opened
	// Tokens is what the item costs in every request while it is on and the
	// kind's mode sends it.
	Tokens int `json:"tokens"`
}

// SwitchState is an item with its switches.
type SwitchState struct {
	SwitchItem
	On   bool   `json:"on"`
	From string `json:"from,omitempty"` // the scope that decided On; "" the default
	// Global, Project, Session are each scope's switch for it ("on", "off",
	// "" when nothing there matches it).
	Global  string `json:"global,omitempty"`
	Project string `json:"project,omitempty"`
	Session string `json:"session,omitempty"`
}

// switchKind is what a kind tells the generic part.
type switchKind struct {
	name    string                   // "skills": the files' name
	modes   []string                 // the modes it takes, the default first
	items   func() []SwitchItem      // what it switches, now
	changed func()                   // after a change: rebuild what it feeds
	about   map[string]string        // each mode in words, for a UI
	note    func(mode string) string // what to say about the mode in force, or ""
	// prepare, when set, runs before a mode is saved: what the mode needs
	// (a summary written), or why it cannot be set.
	prepare func(mode string) error
}

// switchScopes is precedence, highest first.
var switchScopes = []string{ScopeSession, ScopeProject, ScopeGlobal}

// switchFile is a kind's file at scope, "" when the scope has none here (an
// in-memory session, a workspace with no project state).
func (h *Harness) switchFile(kind, scope string) string {
	switch scope {
	case ScopeGlobal:
		return filepath.Join(DunHome(), kind+".json")
	case ScopeProject:
		p, err := EnsureProject(h.configDir())
		if err != nil {
			return ""
		}
		return filepath.Join(p.WriteDir(), kind+".json")
	case ScopeSession:
		if h.cfg.SessionFile == "" {
			return ""
		}
		return strings.TrimSuffix(h.cfg.SessionFile, ".jsonl") + "." + kind + ".json"
	}
	return ""
}

// loadSwitchFile reads one file; missing or unreadable is empty.
func loadSwitchFile(p string) SwitchFile {
	var f SwitchFile
	if p == "" {
		return f
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return f
	}
	if json.Unmarshal(b, &f) != nil {
		log.Printf("dun: %s is not readable; its switches count as unset", p)
		return SwitchFile{}
	}
	return f
}

// switchSet is a kind's files, read once.
type switchSet map[string]SwitchFile

func (h *Harness) switchSet(kind string) switchSet {
	s := switchSet{}
	for _, sc := range switchScopes {
		s[sc] = loadSwitchFile(h.switchFile(kind, sc))
	}
	return s
}

// match is a file's switch for name: an exact key, else the longest glob
// that matches it.
func (f SwitchFile) match(name string) (on, ok bool) {
	if v, hit := f.On[name]; hit {
		return v, true
	}
	best := -1
	for pat, v := range f.On {
		if m, err := path.Match(pat, name); err == nil && m && len(pat) > best {
			on, ok, best = v, true, len(pat)
		}
	}
	return on, ok
}

// on resolves one item: on unless a scope says otherwise; from is the scope
// that decided, at each scope's own switch for it.
func (s switchSet) on(name string) (on bool, from string, at map[string]string) {
	on, at = true, map[string]string{}
	for _, sc := range switchScopes {
		v, ok := s[sc].match(name)
		if !ok {
			continue
		}
		at[sc] = onOffWord(v)
		if from == "" {
			on, from = v, sc
		}
	}
	return on, from, at
}

// mode is the mode in force and the scope that set it ("" the default).
func (s switchSet) mode(def string) (string, string) {
	for _, sc := range switchScopes {
		if m := s[sc].Mode; m != "" {
			return m, sc
		}
	}
	return def, ""
}

// SwitchView is a kind as a UI shows it: its items, its mode at each scope
// and in force, and what each mode means.
type SwitchView struct {
	Kind     string            `json:"kind"`
	Items    []SwitchState     `json:"items"`
	Mode     string            `json:"mode"`
	ModeFrom string            `json:"modeFrom,omitempty"`
	Modes    map[string]string `json:"modes,omitempty"` // scope → its mode, "" unset
	About    map[string]string `json:"about,omitempty"` // mode → what it means
	Order    []string          `json:"order,omitempty"` // the modes, the default first
	Note     string            `json:"note,omitempty"`
}

// switchView builds a kind's view.
func (h *Harness) switchView(k switchKind) SwitchView {
	set := h.switchSet(k.name)
	v := SwitchView{Kind: k.name, Modes: map[string]string{}, About: k.about, Order: k.modes}
	v.Mode, v.ModeFrom = set.mode(k.modes[0])
	for _, sc := range switchScopes {
		v.Modes[sc] = set[sc].Mode
	}
	for _, it := range k.items() {
		on, from, at := set.on(it.Name)
		v.Items = append(v.Items, SwitchState{SwitchItem: it, On: on, From: from,
			Global: at[ScopeGlobal], Project: at[ScopeProject], Session: at[ScopeSession]})
	}
	if k.note != nil {
		v.Note = k.note(v.Mode)
	}
	return v
}

// setSwitch sets pattern (an item's name or a glob) on or off at scope; nil
// unsets it there. It returns what to say.
func (h *Harness) setSwitch(k switchKind, pattern, scope string, on *bool) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("which one? a name or a glob (pdf-*)")
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return "", fmt.Errorf("%q is not a glob: %v", pattern, err)
	}
	matched := 0
	for _, it := range k.items() {
		if m, _ := path.Match(pattern, it.Name); m {
			matched++
		}
	}
	if on != nil && matched == 0 {
		return "", fmt.Errorf("nothing matches %q (/%s lists them)", pattern, strings.TrimSuffix(k.name, "s"))
	}
	if err := h.editSwitchFile(k.name, scope, func(f *SwitchFile) {
		if on == nil {
			delete(f.On, pattern)
			return
		}
		if f.On == nil {
			f.On = map[string]bool{}
		}
		f.On[pattern] = *on
	}); err != nil {
		return "", err
	}
	if k.changed != nil {
		k.changed()
	}
	what := "unset"
	if on != nil {
		what = onOffWord(*on)
	}
	n := fmt.Sprintf("%d %s", matched, strings.TrimSuffix(k.name, "s"))
	if matched != 1 {
		n += "s"
	}
	return fmt.Sprintf("%s %s: %s for %s (%s)", k.name, pattern, what, scopeWords(scope), n), nil
}

// setSwitchMode sets the kind's mode at scope; "" unsets it there.
func (h *Harness) setSwitchMode(k switchKind, mode, scope string) (string, error) {
	if mode != "" && !containsStr(k.modes, mode) {
		return "", fmt.Errorf("mode %q: want %s", mode, strings.Join(k.modes, " or "))
	}
	if k.prepare != nil && mode != "" {
		if err := k.prepare(mode); err != nil {
			return "", err
		}
	}
	if err := h.editSwitchFile(k.name, scope, func(f *SwitchFile) { f.Mode = mode }); err != nil {
		return "", err
	}
	if k.changed != nil {
		k.changed()
	}
	now, _ := h.switchSet(k.name).mode(k.modes[0])
	set := mode
	if set == "" {
		set = "unset"
	}
	return fmt.Sprintf("%s: %s for %s (in force: %s)", k.name, set, scopeWords(scope), now), nil
}

// editSwitchFile reads, edits and writes one file.
func (h *Harness) editSwitchFile(kind, scope string, edit func(*SwitchFile)) error {
	p := h.switchFile(kind, scope)
	if p == "" {
		return fmt.Errorf("there is no %s scope here", scope)
	}
	f := loadSwitchFile(p)
	edit(&f)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// scopeWords is a scope as a phrase.
func scopeWords(scope string) string {
	switch scope {
	case ScopeGlobal:
		return "every project"
	case ScopeProject:
		return "this project"
	case ScopeSession:
		return "this session"
	}
	return scope
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// switchKinds are the kinds a client can switch, by name.
func (h *Harness) switchKinds() map[string]switchKind {
	return map[string]switchKind{"skills": h.skillKind(), contextFilesKind: h.contextFileKind()}
}

// SwitchView is a kind's view for a UI; ok=false for an unknown kind.
func (h *Harness) SwitchView(kind string) (SwitchView, bool) {
	k, ok := h.switchKinds()[kind]
	if !ok {
		return SwitchView{}, false
	}
	return h.switchView(k), true
}

// SetSwitch is setSwitch by kind name.
func (h *Harness) SetSwitch(kind, pattern, scope string, on *bool) (string, error) {
	k, ok := h.switchKinds()[kind]
	if !ok {
		return "", fmt.Errorf("unknown kind %q", kind)
	}
	return h.setSwitch(k, pattern, scope, on)
}

// SetSwitchMode is setSwitchMode by kind name.
func (h *Harness) SetSwitchMode(kind, mode, scope string) (string, error) {
	k, ok := h.switchKinds()[kind]
	if !ok {
		return "", fmt.Errorf("unknown kind %q", kind)
	}
	return h.setSwitchMode(k, mode, scope)
}
