package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun"
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

// /context config: flags in either form, values in tokens or default; the
// settings are in force at once and saved for the model.
func TestContextConfigCmd(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_MAX_OUTPUT_TOKENS", "")
	h := dun.NewTestHarness(dun.Endpoint{URL: "https://litellm.corp/v1", Model: "deepseek-1017"})
	t.Cleanup(func() {
		_, _, _ = contextConfigCmd(context.Background(), h, "--compact-buffer default --tool-lod default --recap-nudge default")
	})
	msg, changed, err := contextConfigCmd(context.Background(), h, "--max 256k --compact-buffer=24k --tool-lod 8k --recap-nudge 30k")
	if err != nil || !changed {
		t.Fatalf("%q %v", msg, err)
	}
	for _, want := range []string{"max 256,000", "compact-buffer 24,000", "tool-lod 8,000", "recap-nudge 30,000", "saved for deepseek-1017"} {
		if !strings.Contains(msg, want) {
			t.Errorf("lacks %q: %s", want, msg)
		}
	}
	if _, _, err := contextConfigCmd(context.Background(), h, "--compact-buffer 1k"); err == nil {
		t.Error("a buffer too small for a reply was taken")
	}
	for _, bad := range []string{"--max", "--size 3", "max 3", "--max lots"} {
		if _, _, err := contextConfigCmd(context.Background(), h, bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
