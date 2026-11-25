package proto

import (
	"strconv"
	"strings"
)

// The kinds of an UntilCond.
const (
	UntilTurns = "turns"
	UntilGoal  = "goal"
	UntilPlan  = "plan"
)

// UntilLine is conds as /until takes them ("turns 10 plan goal \"…\"
// --check \"…\""), "" for none: a client that changes one condition sends
// the others back with it (harness.ParseUntil reads it).
func UntilLine(conds []UntilCond) string {
	var out []string
	for _, c := range conds {
		switch c.Kind {
		case UntilTurns:
			out = append(out, "turns "+strconv.Itoa(c.N))
		case UntilPlan:
			out = append(out, "plan")
		case UntilGoal:
			w := "goal " + untilQuote(c.Goal)
			if c.Check != "" {
				w += " --check " + untilQuote(c.Check)
			}
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// untilQuote quotes s for /until: double quotes, \ and " escaped.
func untilQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
