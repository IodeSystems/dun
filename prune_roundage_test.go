package dun

// roundAge formats a duration as a short age string for session listings.

import (
	"testing"
	"time"
)

func TestRoundAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{3 * time.Hour + 30 * time.Minute, "3h"},
		{24 * time.Hour, "24h"},
		{47 * time.Hour, "47h"},
		{48 * time.Hour, "2 days"},
		{72 * time.Hour, "3 days"},
		{0, "0m"},
	}
	for _, c := range cases {
		if got := roundAge(c.d); got != c.want {
			t.Errorf("roundAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
