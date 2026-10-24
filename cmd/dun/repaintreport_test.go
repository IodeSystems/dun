package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRedactor() redactor {
	return newRedactorFor("/home/alice", []string{"alice", "al"}, []string{"box1.example.net", "box1"})
}

// The deterministic pass: home, user, host, e-mail and a token, each replaced
// at its own width.
func TestRedactDeterministic(t *testing.T) {
	r := testRedactor()
	tok := "ghp_" + strings.Repeat("Ab3", 10)
	in := "│ /home/alice/src/dun on box1 · alice@box1.example.net · key " + tok + " · al │"
	got := r.redact(in)
	for _, gone := range []string{"/home/alice", "box1", "alice", tok} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived: %q", gone, got)
		}
	}
	for _, want := range []string{"~__________/src/dun", " on [hos ", "[email]", "[secret]"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in %q", want, got)
		}
	}
	if !strings.Contains(got, " al │") {
		t.Errorf("a name shorter than %d stays: %q", minNameLen, got)
	}
	if widthOf(got) != widthOf(in) {
		t.Errorf("width %d, was %d:\n%s\n%s", widthOf(got), widthOf(in), in, got)
	}
	// A commit id is lower-case hex, not a secret; a mixed-case run is one.
	commit := "c10430d5f2a1b3c4d5e6f708192a3b4c5d6e7f80"
	if got := r.redact(commit); got != commit {
		t.Errorf("commit id redacted: %q", got)
	}
	mixed := "Zq8" + strings.Repeat("xY7", 11)
	if got := r.redact("token=" + mixed); strings.Contains(got, mixed) {
		t.Errorf("mixed-case run survived: %q", got)
	}
	if len(r.scan("f", got)) != 0 {
		t.Errorf("the scan finds what redact left: %v", r.scan("f", got))
	}
}

func TestFitPlaceholder(t *testing.T) {
	for _, c := range []struct {
		ph   string
		w    int
		want string
	}{{"[user]", 6, "[user]"}, {"[user]", 9, "[user]___"}, {"[user]", 3, "[us"}, {"~", 4, "~___"}} {
		if got := fitPlaceholder(c.ph, c.w); got != c.want {
			t.Errorf("fit(%q,%d) = %q, want %q", c.ph, c.w, got, c.want)
		}
	}
}

func TestLocalEndpoint(t *testing.T) {
	for u, want := range map[string]bool{
		"http://127.0.0.1:8080/v1":        true,
		"http://localhost:11434":          true,
		"http://192.168.1.76:8000/v1":     true,
		"http://10.0.0.5/v1":              true,
		"http://[::1]:8080":               true,
		"http://gpubox:8000/v1":           true,
		"http://gpubox.lan/v1":            true,
		"https://api.openai.com/v1":       false,
		"https://openrouter.ai/api/v1":    false,
		"http://8.8.8.8/v1":               false,
		"":                                false,
		"https://corrallm.example.com/v1": false,
	} {
		if got := localEndpoint(u); got != want {
			t.Errorf("localEndpoint(%q) = %v, want %v", u, got, want)
		}
	}
}

// writeCapture makes a capture directory by hand.
func writeCapture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bugreports", "20261023-120000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The prompt is built from the pre-redacted files, ESC shown as a glyph, and
// nothing the deterministic pass finds reaches it.
func TestPrepareReportPrompt(t *testing.T) {
	r := testRedactor()
	dir := writeCapture(t, map[string]string{
		"expected.ansi": "\x1b[1mdun\x1b[0m /home/alice/src\nready\n",
		"actual.ansi":   "\x1b[1mdun\x1b[0m /home/alice/src\nJUNK\n",
		"diff.txt":      "row 2\n  expected: ready\n  actual:   JUNK\n",
		"state.json":    `{"session":"s1","host":"box1"}`,
		"events.jsonl":  `{"kind":"ev:user","info":"mail alice@box1.example.net"}`,
		"README.txt":    "readme",
	})
	if err := prepareReport(dir, r); err != nil {
		t.Fatal(err)
	}
	p := readFile(t, filepath.Join(dir, "prompt.txt"))
	if !strings.HasPrefix(p, reportPrompt) {
		t.Error("prompt does not start with the instructions")
	}
	if strings.Contains(p, "\x1b") || !strings.Contains(p, escMark+"[1mdun") {
		t.Error("ESC should be sent as " + escMark)
	}
	if leaks := r.scan("prompt.txt", p); len(leaks) != 0 {
		t.Errorf("the prompt leaks: %v", leaks)
	}
	for _, f := range []string{"=== FILE diff.txt ===", "=== FILE actual.ansi ===", "=== FILE events.jsonl ==="} {
		if !strings.Contains(p, f) {
			t.Errorf("prompt lacks %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "prered", "README.txt")); err == nil {
		t.Error("README.txt need not be redacted")
	}
}

// finishReport writes the model's files, derives the .txt screens and the
// diff, and the post-check flags what the model let through and a row whose
// width it changed.
func TestFinishReportFlagsLeak(t *testing.T) {
	r := testRedactor()
	dir := writeCapture(t, map[string]string{
		"expected.ansi": "\x1b[1mdun\x1b[0m [text]\nready\n",
		"actual.ansi":   "\x1b[1mdun\x1b[0m [text]\nJUNK!\n",
		"state.json":    "{}\n",
		"events.jsonl":  "\n",
	})
	if err := prepareReport(dir, r); err != nil {
		t.Fatal(err)
	}
	out := "Sure, here it is.\n" +
		"=== FILE expected.ansi ===\n" + escMark + "[1mdun" + escMark + "[0m [text]\nready\n=== END ===\n" +
		"=== FILE actual.ansi ===\n```\n" + escMark + "[1mdun" + escMark + "[0m /home/alice\nJUNK!\n```\n=== END ===\n" +
		"=== FILE explanation.md ===\n# Junk on row 2\nRow 2 shows JUNK! where dun drew ready.\n=== END ===\n"
	if err := os.WriteFile(filepath.Join(dir, "model-output.txt"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := finishReport(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "redacted", "expected.ansi")); got != "\x1b[1mdun\x1b[0m [text]\nready\n" {
		t.Errorf("redacted/expected.ansi = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "redacted", "actual.txt")); strings.Contains(got, "\x1b") || !strings.Contains(got, "JUNK!") {
		t.Errorf("redacted/actual.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "redacted", "diff.txt")); !strings.Contains(got, "row 2\n  expected: ready\n  actual:   JUNK!") {
		t.Errorf("redacted/diff.txt = %q", got)
	}
	if res.title != "Junk on row 2" || len(res.head) != 2 {
		t.Errorf("title %q head %v", res.title, res.head)
	}
	if len(res.leaks) == 0 || !strings.Contains(res.leaks[0].String(), "redacted/actual.ansi:1: home directory") {
		t.Errorf("leaks = %v, want the home dir in redacted/actual.ansi", res.leaks)
	}
	if len(res.widths) == 0 || !strings.Contains(strings.Join(res.widths, ";"), "actual.ansi row 1") {
		t.Errorf("widths = %v, want row 1 of actual.ansi", res.widths)
	}
	if len(res.fallback) != 2 { // state.json and events.jsonl were not returned
		t.Errorf("fallback = %v", res.fallback)
	}
	msg := stripANSI(res.message())
	for _, want := range []string{"⚠ the check still finds", "gh issue create --web --repo iodesystems/dun --title 'Junk on row 2' --body-file", "Row 2 shows JUNK!"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// /repaint report on a local model sends at once; on a remote one it asks.
func TestRepaintReportAsksForRemoteModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("TMUX", "")
	fakeTmux(t, "", "")
	for _, c := range []struct {
		url  string
		asks bool
	}{{"http://127.0.0.1:8000/v1", false}, {"https://api.example.com/v1", true}} {
		v := tallConvo(t)
		v.m.url, v.m.model = c.url, "m1"
		_ = v.m.View()
		dir, err := v.m.capture(time.Now().Add(time.Duration(len(c.url)) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		nm, _ := v.m.Update(repaintDoneMsg{dir: dir, report: true})
		v.m = nm.(tuiModel)
		if asked := v.m.confirm.q != ""; asked != c.asks {
			t.Errorf("%s: asked=%v (%q)", c.url, asked, v.m.confirm.q)
		}
		if c.asks && v.m.confirm.q != "send the capture to m1 for redaction? y/n" {
			t.Errorf("question = %q", v.m.confirm.q)
		}
		if !c.asks && v.m.reportDir != dir {
			t.Errorf("local: reportDir = %q, want %q", v.m.reportDir, dir)
		}
		if _, err := os.Stat(filepath.Join(dir, "prompt.txt")); err != nil {
			t.Errorf("prompt.txt: %v", err)
		}
	}
}
