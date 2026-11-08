package dun

import (
	"context"
	"strings"
	"testing"
)

// A finished foreground exec that printed nothing says so; one that printed
// something is as it was.
func TestRenderForeground_NoOutput(t *testing.T) {
	for _, tc := range []struct {
		r    ExecResult
		want string
	}{
		{ExecResult{}, "(no output; exit 0)"},
		{ExecResult{Output: "\n"}, "(no output; exit 0)"},
		{ExecResult{Code: 3}, "(no output)\n[exit: status 3]"},
		{ExecResult{Code: -1, Err: "no such file"}, "(no output)\n[exit: no such file]"},
		{ExecResult{Output: "hi\n"}, "hi\n"},
		{ExecResult{Output: "bad", Code: 2}, "bad\n[exit: status 2]"},
	} {
		if got := renderForeground(tc.r); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.r, got, tc.want)
		}
		if tc.r.Output != "" && strings.TrimSpace(tc.r.Output) != "" && renderForeground(tc.r) != tc.r.Render() {
			t.Errorf("%+v: non-empty output changed", tc.r)
		}
	}
}

// Through the tool, on both paths: the bare dispatcher and a session's
// (the job machinery).
func TestExec_NoOutputThroughTheTool(t *testing.T) {
	dir := t.TempDir()
	bare := withExec(nil, &HostShell{Dir: dir}, nil, nil, nil, nil, nil)
	t.Setenv("DUN_HOME", t.TempDir())
	h := startNudge(t, &nudgeRunner{}, -1, -1)
	for name, d := range map[string]func(string) string{
		"bare": func(cmd string) string {
			out, _ := bare(context.Background(), *nudgeCall("c", "exec", `{"command":"`+cmd+`"}`))
			return out
		},
		"session": func(cmd string) string {
			out, _ := h.Session.Dispatch(context.Background(), *nudgeCall("c", "exec", `{"command":"`+cmd+`"}`))
			return out
		},
	} {
		if got := d("true"); got != "(no output; exit 0)" {
			t.Errorf("%s, empty: %q", name, got)
		}
		if got := d("exit 3"); got != "(no output)\n[exit: status 3]" {
			t.Errorf("%s, empty and failed: %q", name, got)
		}
		if got := d("echo hi"); got != "hi\n" {
			t.Errorf("%s, output: %q", name, got)
		}
	}
}
