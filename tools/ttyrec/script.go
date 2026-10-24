package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// A script drives a recording. It is a walk file: steps, each a list of
// actions and then a capture marker named by the step's id. Fields ttyrec
// does not use (a step's promise, a walk's scenario) are carried for the
// tools that read the same file.
//
//	{"cols": 100, "rows": 30, "steps": [
//	  {"id": "it-says-what-it-needs", "promise": "…", "actions": [
//	    {"wait_for": "ask dun"},
//	    {"type": "hello"}, {"key": "enter"},
//	    {"wait_for": "not logged in", "timeout_ms": 10000},
//	    {"sleep_ms": 300}
//	  ]}
//	]}
//
// type text has ${VAR} expanded from the environment. key takes one name or a
// space-separated list (keyBytes). After the actions a step waits for the
// screen to be quiet (settle_ms, default 700) before its marker.
type script struct {
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	TypingMS int    `json:"typing_ms"` // per character; default 40
	Steps    []step `json:"steps"`
}

type step struct {
	ID       string   `json:"id"`
	Promise  string   `json:"promise,omitempty"`
	Actions  []action `json:"actions"`
	SettleMS int      `json:"settle_ms,omitempty"`
	NoMark   bool     `json:"no_mark,omitempty"` // drive only; no capture
}

type action struct {
	Type      *string `json:"type,omitempty"`
	Key       string  `json:"key,omitempty"`
	WaitFor   string  `json:"wait_for,omitempty"`
	WaitGone  string  `json:"wait_gone,omitempty"`
	TimeoutMS int     `json:"timeout_ms,omitempty"`
	SleepMS   int     `json:"sleep_ms,omitempty"`
	// Say is how the action reads in the step's log ("typed the address my
	// colleague gave me"); empty means it is described from the action.
	Say string `json:"say,omitempty"`
}

func loadScript(path string) (*script, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s script
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, st := range s.Steps {
		if st.ID == "" {
			return nil, fmt.Errorf("%s: step %d has no id", path, i+1)
		}
		if seen[st.ID] {
			return nil, fmt.Errorf("%s: step id %q twice", path, st.ID)
		}
		seen[st.ID] = true
		for j, a := range st.Actions {
			n := 0
			for _, set := range []bool{a.Type != nil, a.Key != "", a.WaitFor != "", a.WaitGone != "", a.SleepMS > 0} {
				if set {
					n++
				}
			}
			if n != 1 {
				return nil, fmt.Errorf("%s: step %q action %d: want exactly one of type, key, wait_for, wait_gone, sleep_ms", path, st.ID, j+1)
			}
			if a.Key != "" {
				if _, err := keyBytes(a.Key); err != nil {
					return nil, fmt.Errorf("%s: step %q: %w", path, st.ID, err)
				}
			}
		}
	}
	return &s, nil
}

var namedKeys = map[string]string{
	"enter": "\r", "return": "\r", "tab": "\t", "shift+tab": "\x1b[Z", "esc": "\x1b", "escape": "\x1b",
	"space": " ", "backspace": "\x7f", "delete": "\x1b[3~",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F", "pgup": "\x1b[5~", "pgdn": "\x1b[6~",
	"alt+enter": "\x1b\r",
}

// keyBytes encodes space-separated key names: the names above, ctrl+<letter>.
func keyBytes(spec string) ([]byte, error) {
	var out []byte
	for _, k := range strings.Fields(spec) {
		k = strings.ToLower(k)
		if s, ok := namedKeys[k]; ok {
			out = append(out, s...)
			continue
		}
		if l, ok := strings.CutPrefix(k, "ctrl+"); ok && len(l) == 1 && l[0] >= 'a' && l[0] <= 'z' {
			out = append(out, l[0]-'a'+1)
			continue
		}
		return nil, fmt.Errorf("unknown key %q", k)
	}
	return out, nil
}

// keyWords is how a key reads in a log: "Enter", "Shift+Tab", "the down arrow".
func keyWords(spec string) string {
	var ws []string
	for _, k := range strings.Fields(spec) {
		switch strings.ToLower(k) {
		case "up", "down", "left", "right":
			ws = append(ws, "the "+strings.ToLower(k)+" arrow")
		case "esc", "escape":
			ws = append(ws, "Esc")
		default:
			parts := strings.Split(k, "+")
			for i, p := range parts {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
			ws = append(ws, strings.Join(parts, "+"))
		}
	}
	return strings.Join(ws, ", ")
}
