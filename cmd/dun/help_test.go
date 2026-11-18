package main

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// --help fits the width it is given (stderr is no terminal here, so
// $COLUMNS), leaves out the advanced flags and points to --help-all, which
// lists them; a --x/--no-x pair is one entry, and a zero default is not
// printed.
func TestHelp_WidthGroupsAndDefaults(t *testing.T) {
	t.Parallel() // runs the built dun (testDun): its own process and env
	adv := flagGroups[len(flagGroups)-1].names
	adv = append(adv, "worker", "settle-delay", "non-interactive") // ungrouped: listed as advanced
	for _, width := range []int{80, 50, 120} {
		cols := []string{"COLUMNS=" + strconv.Itoa(width)}
		_, help, code := runDun(t, "", cols, "--help")
		_, all, codeAll := runDun(t, "", cols, "--help-all")
		if code != 0 || codeAll != 0 {
			t.Fatalf("width %d: exit %d, %d\n%s", width, code, codeAll, help)
		}
		for name, out := range map[string]string{"--help": help, "--help-all": all} {
			for _, l := range strings.Split(out, "\n") {
				if utf8.RuneCountInString(l) > width {
					t.Errorf("%s at %d: a line of %d: %q", name, width, utf8.RuneCountInString(l), l)
				}
			}
			for _, bad := range []string{"(default 0)", "(default [])", "(default true)", "(default false)", "\n  --no-rag", "\n  --no-lsp", "--ship", "--help-all\n      "} {
				if strings.Contains(out, bad) {
					t.Errorf("%s at %d: has %q", name, width, bad)
				}
			}
			if !strings.Contains(out, "\n  --rag, --no-rag\n") || !strings.Contains(out, "\n  --timeout duration\n") {
				t.Errorf("%s at %d: no merged --rag entry or typed --timeout:\n%s", name, width, out)
			}
		}
		for _, n := range adv {
			if strings.Contains(help, "\n  --"+n+"\n") || strings.Contains(help, "\n  --"+n+" ") {
				t.Errorf("--help at %d lists the advanced --%s", width, n)
			}
			if !strings.Contains(all, "\n  --"+n+"\n") && !strings.Contains(all, "\n  --"+n+" ") {
				t.Errorf("--help-all at %d lacks --%s", width, n)
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(help), "dun --help-all") || strings.Contains(all, "more flags") {
			t.Errorf("width %d: the pointer to --help-all is wrong:\n%s", width, help)
		}
		if !strings.Contains(help, `dun -p "fix the test"`) {
			t.Errorf("width %d: the start block has no -p example", width)
		}
	}
}
