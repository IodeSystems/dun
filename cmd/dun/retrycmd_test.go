package main

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// /retry: presets and knobs set the client at once, and the config saved
// carries them in the fields a later start reads.
func TestRetryCmd(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := llm.NewClient("http://x/v1", "", "m")
	if msg, save, err := retryCmd(c, ""); err != nil || save != nil || !strings.Contains(msg, "up to 5 tries") {
		t.Fatalf("status: %q %v", msg, err)
	}
	_, save, err := retryCmd(c, "stubborn")
	if err != nil || c.Retry5xxAttempts != 20 || c.RetryBudget != time.Hour || c.TurnRetryBudget != 2*time.Hour {
		t.Fatalf("stubborn: %+v %v", c, err)
	}
	if save.Retry5xxAttempts != 20 || save.RetryBudget != "1h0m0s" || save.TurnRetryBudget != "2h0m0s" {
		t.Errorf("saved: %+v", save)
	}
	if rb, tb, rs, ts := parseRetryBudgets(*save); !rs || !ts || rb != time.Hour || tb != 2*time.Hour {
		t.Errorf("a later start reads %v %v", rb, tb)
	}
	for _, a := range []string{"attempts 7", "budget unbounded", "turn off"} {
		if _, _, err := retryCmd(c, a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	if c.Retry5xxAttempts != 7 || c.RetryBudget >= 0 || !c.DisableTurnRetry {
		t.Errorf("knobs: %+v", c)
	}
	if _, save, _ := retryCmd(c, "turn off"); save.TurnRetryBudget != "0" || save.RetryBudget != "-1s" {
		t.Errorf("off and unbounded saved as %q %q", save.TurnRetryBudget, save.RetryBudget)
	}
	for _, bad := range []string{"attempts 0", "budget soon", "turn -5m", "fast"} {
		if _, _, err := retryCmd(c, bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
