package dun

import (
	"reflect"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// Every field agentkit's PolicyFor sets must be set in dun's policy too. This
// is the aw4 failure mode — a field nobody set, a window blown twice in a day —
// turned into a test: a new agentkit knob fails here until dun decides on it.
func TestShaperPolicy_CoversPolicyFor(t *testing.T) {
	ref := reflect.ValueOf(agent.PolicyFor(200_000))
	ours := reflect.ValueOf(shaperPolicy(200_000))
	for i := 0; i < ref.NumField(); i++ {
		f := ref.Type().Field(i)
		if !f.IsExported() || ref.Field(i).IsZero() {
			continue
		}
		if ours.Field(i).IsZero() {
			t.Errorf("agent.PolicyFor sets %s=%v but dun's shaperPolicy leaves it zero", f.Name, ref.Field(i).Interface())
		}
	}
}

// An unknown window keeps every non-budget rung: that is why dun does not
// build from PolicyFor, which returns an empty policy for window 0.
func TestShaperPolicy_UnknownWindowKeepsLOD(t *testing.T) {
	p := shaperPolicy(0)
	if p.BudgetTokens != 0 {
		t.Errorf("unknown window must be unbudgeted, got %d", p.BudgetTokens)
	}
	if p.LODTruncateAboveChars == 0 || p.MaxToolResultChars == 0 || p.PreserveLastMessages == 0 {
		t.Errorf("non-budget rungs must survive an unknown window: %+v", p)
	}
}
