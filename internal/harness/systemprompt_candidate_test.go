package harness

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// systemFinishCandidate is a CANDIDATE for arm B of the done-after-checks A/B,
// drafted from the four premature-done failure modes measured on TB2.1
// (jobs/full-20261106-232928): a test claimed to have passed that did not, a
// missing interpreter given up on, done claimed with the work unfinished, a
// report missing a required key. The sweep decides what lands; until then the
// candidate must sit beside systemFinish and touch no prompt. This test pins
// that, and pins the clause targeting each failure mode, so a later edit of
// the candidate cannot silently drop the thing the sweep would measure.
func TestSystemFinishCandidate_BesideSystemFinish(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: mcpserver.ServerShell}}
	for _, alone := range []bool{false, true} {
		s := systemFor(tools, nil, nil, "", alone)
		if strings.Contains(s, systemFinishCandidate) {
			t.Errorf("alone=%v: the candidate is in the prompt; arm A is no longer untouched", alone)
		}
		if !strings.Contains(s, systemFinish) {
			t.Errorf("alone=%v: the current work line is gone", alone)
		}
	}
	// One clause per measured failure mode, in the order the modes were
	// measured. A candidate that loses one of these is a different candidate.
	for clause, mode := range map[string]string{
		"not a reason to stop":                                        "gave up on a missing interpreter",
		"do not report done with a step left undone":                  "claimed done, work unfinished",
		"keys, values":                                                "report missing a required key",
		"a check you only read is not a check you passed":             "claimed a test passed",
		"end only once every stated requirement is met and its check passes": "the turn stays open on failure",
	} {
		if !strings.Contains(systemFinishCandidate, clause) {
			t.Errorf("candidate lost the clause for %q: %q", mode, clause)
		}
	}
	// The measured wording failure (winning-avg-corewars): "check, then give
	// your final message" made the model stop while admitting a failing
	// requirement. The candidate must not carry that phrasing.
	if strings.Contains(systemFinishCandidate, "then give your final message") {
		t.Error("candidate uses the corewars wording that closed the turn")
	}
}
