package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := dunConfig{URL: "http://x:9", Model: "my-model", Key: "sk-abcd1234"}
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(); !reflect.DeepEqual(got, c) {
		t.Fatalf("round-trip mismatch: %+v != %+v", got, c)
	}
	// The key is secret → 0600.
	st, err := os.Stat(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %v, want 0600", perm)
	}
}

func TestLoadConfig_missingIsZero(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if got := loadConfig(); !reflect.DeepEqual(got, dunConfig{}) {
		t.Fatalf("missing config should be zero, got %+v", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "c"); got != "c" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]string{"": "(none)", "abcd": "****", "sk-abcd1234": "****1234"}
	for in, want := range cases {
		if got := maskKey(in); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRetryBudgets(t *testing.T) {
	cases := []struct {
		name        string
		fc          dunConfig
		wantReq     time.Duration
		wantTurn    time.Duration
		wantReqSet  bool
		wantTurnSet bool
	}{
		{"empty is unset", dunConfig{}, 0, 0, false, false},
		{"request only", dunConfig{RetryBudget: "1h"}, time.Hour, 0, true, false},
		{"turn zero disables", dunConfig{TurnRetryBudget: "0"}, 0, 0, false, true},
		{"both", dunConfig{RetryBudget: "-1s", TurnRetryBudget: "30m"}, -time.Second, 30 * time.Minute, true, true},
	}
	for _, tc := range cases {
		req, turn, reqSet, turnSet := parseRetryBudgets(tc.fc)
		if req != tc.wantReq || turn != tc.wantTurn || reqSet != tc.wantReqSet || turnSet != tc.wantTurnSet {
			t.Errorf("%s: got (%v %v %v %v), want (%v %v %v %v)", tc.name, req, turn, reqSet, turnSet,
				tc.wantReq, tc.wantTurn, tc.wantReqSet, tc.wantTurnSet)
		}
	}
	// Junk is ignored, not fatal.
	if _, _, reqSet, turnSet := parseRetryBudgets(dunConfig{RetryBudget: "soon", TurnRetryBudget: "-5m"}); reqSet || turnSet {
		t.Errorf("junk applied: reqSet=%v turnSet=%v", reqSet, turnSet)
	}
}
