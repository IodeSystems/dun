package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Round 4 of the onboarding walk (plan item 37).

func TestNameFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example.com/v1":     "example",
		"https://llm.iodesystems.com/v1": "iodesystems",
		"https://openrouter.ai/api/v1":   "openrouter",
		"http://127.0.0.1:45445/v1":      "local",
		"http://localhost:11434":         "local",
		"http://[::1]:8080":              "local",
		"http://gpu-box:8000/v1":         "gpu-box",
		"https://":                       "",
		"api.example.com":                "",
	} {
		if got := nameFromURL(in); got != want {
			t.Errorf("nameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// engineCmdsNow is the engine's command list as the TUI receives it.
func engineCmdsNow(t *testing.T) []engineCmd {
	t.Helper()
	b, err := json.Marshal(commandList(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	var ev evMsg
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return engineCmdsFromEvent(ev)
}

// /help <command> says the command in full: what it does, how to turn it on,
// and its choices. The extras say it in plain words.
func TestHelp_OneCommandInFull(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.engineCmds = engineCmdsNow(t)
	for name, wants := range map[string][]string{
		"rag":      {"Project search (raglit)", "/rag on — turn it on for this session", "/rag auto — turn it on in every session", "include"},
		"lsp":      {"Code navigation (poly-lsp)", "/lsp auto", "/lsp add <name>"},
		"/mcp":     {"preset", "playwright", "same name again replaces it", "add"},
		"login":    {"/login", "connect"},
	} {
		m.convo = nil
		m.showCommandHelp(name)
		got := stripANSI(convoText(m))
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("/help %s lacks %q:\n%s", name, w, got)
			}
		}
	}
	m.convo = nil
	m.showCommandHelp("nosuch")
	if got := stripANSI(convoText(m)); !strings.Contains(got, "no command /nosuch") {
		t.Errorf("/help nosuch: %q", got)
	}
}

// The command list says /mcp and /docker in plain words.
func TestCommands_PlainDescriptions(t *testing.T) {
	for _, c := range engineCmdsNow(t) {
		for _, jargon := range []string{"bare", "switch table", "exec backend"} {
			if strings.Contains(c.desc, jargon) {
				t.Errorf("/%s says %q: %q", c.name, jargon, c.desc)
			}
		}
	}
}

// Not logged in: the conversation says so once, in the attention colour;
// the status line does not say it again.
func TestStatus_NotLoggedInSaidOnce(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	view := stripANSI(m.View().Content)
	if n := strings.Count(view, "not logged in"); n != 1 {
		t.Errorf("\"not logged in\" is on the screen %d times:\n%s", n, view)
	}
}

// The question kept from before the login comes back in the composer as
// just the question; the status line says whose it is until it is changed
// or sent.
func TestRestoredQuestion_IsMarked(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	m = typeStr(m, "Can you help?")
	m = key(m, kEnter)
	held := m.convo[len(m.convo)-1].collapsed
	if !strings.Contains(held, "214") { // an instruction: the attention colour (run 6)
		t.Errorf("the kept-question line is not in the attention colour: %q", held)
	}
	m.switchModel(providerConfig{Name: "work", URL: "http://llm/v1"}, "chat-standard", true)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	if m.input.Value() != "Can you help?" {
		t.Fatalf("composer holds %q", m.input.Value())
	}
	if v := stripANSI(m.View().Content); !strings.Contains(v, "your earlier question") {
		t.Errorf("the status line does not mark the kept question:\n%s", v)
	}
	m = typeStr(m, " now")
	if v := stripANSI(m.View().Content); strings.Contains(v, "your earlier question") {
		t.Error("still marked after it was changed")
	}
}

// A status notice has a blank row above and a blank row below.
func TestNotice_BlankAboveAndBelow(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("hello")
	i := v.m.appendNotice(convoEntry{collapsed: okNotice("notes added", "running")})
	v.send("next")
	if !v.m.convo[i-1].blank || !v.m.convo[i+1].blank {
		t.Errorf("around the notice: blank above %v, blank below %v\n%s", v.m.convo[i-1].blank, v.m.convo[i+1].blank, v.convo())
	}
	if v.m.convo[i+2].userText != "next" {
		t.Errorf("more than one blank row below the notice:\n%s", v.convo())
	}
}

// Orange is for what is blocked or needs the person: a plain reply is not.
func TestReplyLine_OrangeOnlyForAttention(t *testing.T) {
	if got := replyLine("docker: on"); strings.Contains(got, "214") {
		t.Errorf("a plain reply is orange: %q", got)
	}
	for _, l := range []string{"notes did not start: exit 1", "usage: /mcp on <id>", "unknown server x"} {
		if got := replyLine(l); !strings.Contains(got, "214") && got == l {
			t.Errorf("a failure is not marked: %q", got)
		}
	}
}

// The question band is in the answer's foreground (not khaki), and its fill
// stops at bandCap columns on a wide screen; wrapped rows fill to the same
// edge.
func TestQuestionBand_AnswerColourAndCappedFill(t *testing.T) {
	if answerFG != "252" {
		t.Errorf("answerFG = %q, want glamour's dark document colour 252", answerFG)
	}
	if got := stUser.GetForeground(); got != lipgloss.Color(answerFG) {
		t.Errorf("the band's text colour is %v, want the answer's %q", got, answerFG)
	}
	long := strings.Repeat("word ", 40)
	for _, w := range []int{48, 100, 180} {
		for _, text := range []string{"hi", long} {
			rows := strings.Split(userRowAt(text, w), "\n")
			want := min(w, bandCap)
			for i, r := range rows {
				if got := ansi.StringWidth(r); got != want {
					t.Errorf("width %d, %d-char question, row %d: fill %d, want %d", w, len(text), i, got, want)
				}
			}
		}
	}
}
