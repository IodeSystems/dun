package dundcli

import (
	"slices"
	"testing"
)

func TestTuiArgsOpensThePicker(t *testing.T) {
	got := tuiArgs("/h")
	if !slices.Contains(got, "--pick") || !slices.Contains(got, "--tui") || !slices.Contains(got, "--disable-exit") {
		t.Fatalf("%v", got)
	}
}
