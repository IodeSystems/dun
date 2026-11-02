package numfmt

import (
	"testing"
)

func TestGrouped(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567", -45678: "-45,678"} {
		if got := Grouped(in); got != want {
			t.Errorf("Grouped(%d) = %q, want %q", in, got, want)
		}
	}
	if got := Grouped(uint64(18446744073709551615)); got != "18,446,744,073,709,551,615" {
		t.Errorf("max uint64: %q", got)
	}
}

func TestCommaf(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{Commaf("%d tokens", 128000), "128,000 tokens"},
		{Commaf("%d of %d budget (%d%%)", 96000, 120000, 80), "96,000 of 120,000 budget (80%)"},
		{Commaf("~%d tok · %.0f%% hit · %s", 4321, 87.5, "x"), "~4,321 tok · 88% hit · x"},
		{Commaf("[%8d]", 12345), "[  12,345]"},
		{Commaf("[%-8d]", 12345), "[12,345  ]"},
		{Commaf("%05d", 42), "00042"}, // a padded number is left alone
		{Commaf("%v and %d", 1234, 5678), "1234 and 5,678"},
		{Commaf("%d", "not a number"), "%!d(string=not a number)"},
		{Commaf("trailing %"), "trailing %"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
