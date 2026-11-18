package textfmt

import "testing"

const invokeCall = "<invoke name=\"exec\">\n<parameter name=\"command\">ls</parameter>\n</invoke>"

func TestToolMarkup(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"<｜DSML｜function_calls>\n<｜DSML｜invoke name=\"exec\">", "DSML markup"},
		{"<|DSML|invoke name=\"exec\">", "DSML markup"},
		{"<｜tool▁calls▁begin｜>", "<|tool▁calls▁begin|>"},
		{invokeCall, "<invoke>/<parameter>"},
		{"<function_calls>\n<invoke>\n</function_calls>", "<function_calls>"},
		{"<tool_call>\n  {\"name\": \"exec\", \"arguments\": {}}\n</tool_call>", "<tool_call>"},

		// A tag named in prose is not a call.
		{"Qwen wraps calls in <tool_call> tags", ""},
		{"fix <tool_call> parsing", ""},
		{"<tool_call> then {} with no name", ""},
		{"the <invoke> step and a <parameter> tag", ""},
		{"<function_calls> alone", ""},
		{"a < b and no tags", ""},

		// Code is prose about the tags.
		{"Use `<tool_call>{\"name\":1}` or `` <|DSML|x `` here.", ""},
		{"```xml\n" + invokeCall + "\n```", ""},
		{"~~~\n<｜DSML｜invoke>\n~~~", ""},
		{"Before:\n\n    <｜DSML｜invoke name=\"x\">\n    </｜DSML｜invoke>", ""}, // indented code
		{"> ```\n> <｜DSML｜invoke>\n> ```", ""},                                // fence in a quote
		{"- example:\n  ```\n  <｜DSML｜invoke>\n  ```", ""},                    // fence in a list item
		{"````\n```\n<｜DSML｜invoke>\n````", ""},                               // ``` does not close ````
		{"```\n<｜DSML｜invoke>\n~~~\n<|DSML|x\n```", ""},                       // ~~~ does not close ```
		{"```\nx\n```\n<｜DSML｜invoke>", "DSML markup"},                        // after the fence closed
		{"- item\n\n      <｜DSML｜invoke>", "DSML markup"},                     // list continuation, not code
	} {
		if got := ToolMarkup(c.in); got != c.want {
			t.Errorf("ToolMarkup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFenceToolMarkup(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"paragraph",
			"Checking.\n\n" + invokeCall + "\n\nThen more.",
			"Checking.\n\n```\n" + invokeCall + "\n```\n\nThen more."},
		{"only the span, not prose between two",
			"A.\n\n" + invokeCall + "\n\nLong prose with a <invoke> word.\n\nB.\n\n" + invokeCall,
			"A.\n\n```\n" + invokeCall + "\n```\n\nLong prose with a <invoke> word.\n\nB.\n\n```\n" + invokeCall + "\n```"},
		{"multi-paragraph parameter value",
			"<invoke name=\"x\">\n<parameter name=\"content\">a\n\nb</parameter>\n</invoke>\n\nDone.",
			"```\n<invoke name=\"x\">\n<parameter name=\"content\">a\n\nb</parameter>\n</invoke>\n```\n\nDone."},
		{"blockquote keeps its prefix",
			"> Note:\n>\n> <｜DSML｜invoke name=\"x\">\n> </｜DSML｜invoke>\n\nAfter.",
			"> Note:\n>\n> ```\n> <｜DSML｜invoke name=\"x\">\n> </｜DSML｜invoke>\n> ```\n\nAfter."},
		{"list marker line left alone",
			"- <｜DSML｜invoke name=\"x\">\n  </｜DSML｜invoke>",
			"- <｜DSML｜invoke name=\"x\">\n  </｜DSML｜invoke>"},
		{"longer fence than the span's",
			"<tool_call>{\"name\":\"x\",\"q\":\"```\"}\n</tool_call>",
			"````\n<tool_call>{\"name\":\"x\",\"q\":\"```\"}\n</tool_call>\n````"},
		{"no markup", "plain `<tool_call>` prose", "plain `<tool_call>` prose"},
	} {
		if got := FenceToolMarkup(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
