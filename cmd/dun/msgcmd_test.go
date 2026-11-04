package main

import (
	"strings"
	"testing"
)

// One line is FROM>MESSAGE; several are fenced by a hash no line equals.
func TestMsgLine(t *testing.T) {
	if got := msgLine("a", "hi"); got != "a>hi\n" {
		t.Errorf("one line: %q", got)
	}
	got := msgLine("b", "x\ny")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "b><") || lines[1] != "x" || lines[2] != "y" ||
		lines[3] != "</"+strings.TrimSuffix(strings.TrimPrefix(lines[0], "b><"), ">")+">" {
		t.Errorf("several lines: %q", got)
	}
}
