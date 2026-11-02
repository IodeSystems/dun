package main

import (
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dunconfig"
)

func TestParseRetryBudgets(t *testing.T) {
	cases := []struct {
		name        string
		fc          dunconfig.DunConfig
		wantReq     time.Duration
		wantTurn    time.Duration
		wantReqSet  bool
		wantTurnSet bool
	}{
		{"empty is unset", dunconfig.DunConfig{}, 0, 0, false, false},
		{"request only", dunconfig.DunConfig{RetryBudget: "1h"}, time.Hour, 0, true, false},
		{"turn zero disables", dunconfig.DunConfig{TurnRetryBudget: "0"}, 0, 0, false, true},
		{"both", dunconfig.DunConfig{RetryBudget: "-1s", TurnRetryBudget: "30m"}, -time.Second, 30 * time.Minute, true, true},
	}
	for _, tc := range cases {
		req, turn, reqSet, turnSet := parseRetryBudgets(tc.fc)
		if req != tc.wantReq || turn != tc.wantTurn || reqSet != tc.wantReqSet || turnSet != tc.wantTurnSet {
			t.Errorf("%s: got (%v %v %v %v), want (%v %v %v %v)", tc.name, req, turn, reqSet, turnSet,
				tc.wantReq, tc.wantTurn, tc.wantReqSet, tc.wantTurnSet)
		}
	}
	// Junk is ignored, not fatal.
	if _, _, reqSet, turnSet := parseRetryBudgets(dunconfig.DunConfig{RetryBudget: "soon", TurnRetryBudget: "-5m"}); reqSet || turnSet {
		t.Errorf("junk applied: reqSet=%v turnSet=%v", reqSet, turnSet)
	}
}
