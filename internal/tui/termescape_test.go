package tui

import (
	"regexp"
	"strings"
	"testing"
)

// Model, tool and server text reaches the screen as text: no escape the
// terminal would act on (OSC 52 clipboard write, title, cursor move, clear),
// no C1 control, no bidi control. dun's own styling (SGR) is kept.
const evilText = "A\x1b]52;c;SGVsbG8=\x07B\x1b[2JC\x9b31mD‮E\x1b]0;title\x07F"

var notSGR = regexp.MustCompile(`\x1b[^\[]|\x1b\[[0-9;:]*[^0-9;:m]`)

func assertClean(t *testing.T, what, frame string) {
	t.Helper()
	if m := notSGR.FindString(frame); m != "" {
		t.Errorf("%s: terminal escape %q reaches the frame", what, m)
	}
	for _, r := range frame {
		if (r >= 0x80 && r <= 0x9f) || r == 0x07 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Errorf("%s: control rune %U reaches the frame", what, r)
		}
	}
}

func TestTUI_ModelTextHasNoTerminalEscapes(t *testing.T) {
	v := newVtui(100, 40)
	v.m.proc = &dunProc{stdin: &echoWC{}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	v.event(map[string]any{"type": "turn"})
	v.event(map[string]any{"type": "reasoning", "text": "think " + evilText})
	v.event(map[string]any{"type": "token", "text": "answer " + evilText + "\n\n```\ncode " + evilText + "\n```\n"})
	v.tick()
	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "echo " + evilText}})
	v.event(map[string]any{"type": "tool_result", "tool": "exec", "result": "out " + evilText})
	v.event(map[string]any{"type": "notification", "text": "note " + evilText})
	v.event(map[string]any{"type": "done"})
	v.tick()
	frame := v.m.View().Content // raw: the bytes the terminal gets
	assertClean(t, "frame", frame)
	assertClean(t, "convo", convoText(v.m))
	if !strings.Contains(stripANSI(frame), "answer A") {
		t.Errorf("the text itself is gone:\n%s", frame)
	}
}

func TestTUI_AskTextHasNoTerminalEscapes(t *testing.T) {
	v := newVtui(100, 40)
	v.m.proc = &dunProc{stdin: &echoWC{}}
	v.event(map[string]any{"type": "ready", "tools": []any{"ask_user"}})
	v.send("start")
	v.m.busy = true
	v.event(map[string]any{"type": "ask", "question": "Q " + evilText, "options": []any{"opt " + evilText, "plain"}})
	if !v.m.asking {
		t.Fatal("picker not open")
	}
	assertClean(t, "ask panel", v.m.askPanel())
	assertClean(t, "frame", v.m.View().Content)
}

func TestCleanEvent_KeepsOrdinaryText(t *testing.T) {
	in := evMsg{"type": "token", "text": "tab\there 😀 👨‍👩‍👧 café\nline",
		"items": []any{map[string]any{"content": "x\ty"}}, "n": 3}
	out := cleanEvent(in)
	if out["text"] != in["text"] || out["n"] != 3 {
		t.Errorf("changed: %#v", out)
	}
	if out["items"].([]any)[0].(map[string]any)["content"] != "x\ty" {
		t.Errorf("nested changed: %#v", out["items"])
	}
	got := cleanEvent(evMsg{"text": evilText, "l": []any{evilText}})
	for _, s := range []string{got["text"].(string), got["l"].([]any)[0].(string)} {
		if strings.ContainsAny(s, "\x1b\x07\x9b") || strings.ContainsRune(s, '‮') {
			t.Errorf("not cleaned: %q", s)
		}
	}
}
