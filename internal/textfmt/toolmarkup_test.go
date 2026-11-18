package textfmt

import (
	"strings"
	"testing"
)

func TestToolMarkup(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"<｜DSML｜function_calls>\n<｜DSML｜invoke name=\"exec\">", "DSML markup"},
		{"<|DSML|invoke name=\"exec\">", "DSML markup"},
		{"<invoke name=\"exec\">\n<parameter name=\"command\">ls</parameter>\n</invoke>", "<invoke>/<parameter>"},
		{"<function_calls>", "<function_calls>"},
		{"<tool_call>{\"name\":\"exec\"}</tool_call>", "<tool_call>"},
		{"<｜tool▁calls▁begin｜>", "<|tool▁calls▁begin|>"},
		// Code is prose about the tags.
		{"Use `<tool_call>` or `` <invoke> `` with `<parameter>`.", ""},
		{"```xml\n<invoke name=\"x\"><parameter name=\"y\"/></invoke>\n```", ""},
		{"~~~\n<｜DSML｜invoke>\n~~~", ""},
		// <invoke alone is not a call.
		{"the <invoke> step", ""},
		{"a < b and no tags", ""},
	} {
		if got := ToolMarkup(c.in); got != c.want {
			t.Errorf("ToolMarkup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFenceToolMarkup(t *testing.T) {
	in := "Checking.\n\n<invoke name=\"exec\">\n<parameter name=\"command\">ls</parameter>\n</invoke>\n\nThen more."
	want := "Checking.\n\n```\n<invoke name=\"exec\">\n<parameter name=\"command\">ls</parameter>\n</invoke>\n```\n\nThen more."
	if got := FenceToolMarkup(in); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// A span with a backtick fence inside gets a longer fence.
	in = "<tool_call>\n```\nx\n```\n</tool_call>"
	if got := FenceToolMarkup(in); !strings.HasPrefix(got, "````\n") {
		t.Errorf("fence not longer than the span's: %q", got)
	}
	// No markup: unchanged.
	if in := "plain `<tool_call>` prose"; FenceToolMarkup(in) != in {
		t.Errorf("changed text with no markup")
	}
}
