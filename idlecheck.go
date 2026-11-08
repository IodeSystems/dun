package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// idlecheck.go — what dun says when the model is about to end a turn
// (agent.Session.OnIdle). Asked once per turn, and once more after a file
// edit or write or an exec made since (idleRearm); never a third time.
//
// --non-interactive: no one reviews the result, so a turn that did work (more
// than one round) ends only after the model has checked the task's stated
// requirements against what it produced. Measured need: TB2.1 2026-11-07,
// nginx-request-logging put a directive in the wrong file the task named;
// the system prompt's finish-and-check line alone did not catch it.
//
// Interactive: the person is there and reads the final message, so no extra
// round, unless the plan still has open nodes: the model then finishes them,
// or marks them done or dropped (USER 2026-11-07). Both modes list the open
// nodes with their done_when.

// idleChecklist is the --non-interactive check.
// The wording keeps the turn open when a requirement fails: TB2.1
// winning-avg-corewars (2026-11-07) found 35% where 75% was required, wrote
// "I should attempt to improve stone before finishing — but I'm in an
// idle_check asking to verify and give final message", and stopped; pi kept
// working and passed.
const idleChecklist = "Before you end the turn: list each requirement the task states (file paths, names, formats, values, behaviors) and check each against what you produced now: run it where it can run, read it only where it cannot. If any is not met and you can still work on it, do not end the turn: keep working until it is met. End the turn only when every requirement is met, or when one cannot be met; then say which and why."

// idlePlanOpen is the note when the main agent's plan has open nodes: each
// one's title and done_when, so the model checks the plan's own terms, not
// only the task's (USER 2026-11-07).
const idlePlanOpen = "Open plan nodes:\n%s\nFinish each, or mark it done with how done_when was met, or drop it with why."

// idlePlanMax is how many open nodes the note lists; plan(show) has the rest.
const idlePlanMax = 10

// idleOpenNodes lists the main agent's open nodes, one line each: id, title,
// done_when. "" with none.
func (h *Harness) idleOpenNodes() string {
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return ""
	}
	var lines []string
	more := 0
	for _, n := range g.Nodes {
		if n.Owner != "" || !planOpen(n) {
			continue
		}
		if len(lines) == idlePlanMax {
			more++
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s; done_when: %s", n.ID, n.Title, n.DoneWhen))
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more (plan show)", more))
	}
	return strings.Join(lines, "\n")
}

// idleChecksMax is how many checks one turn gets: the first, and one more
// after a change made since it (idleRearm). No more, so a model and the
// check cannot ping-pong.
const idleChecksMax = 2

// idleRearm starts the second check; %s is the change.
const idleRearm = "You %s after the last check: check the requirements again against what is there now."

func (h *Harness) onIdle(_ context.Context, rounds, asks int) string {
	if asks >= idleChecksMax {
		return ""
	}
	var change string
	if asks > 0 {
		// Checked once: again only after a change (full5
		// break-filter-js-from-html, 2026-11-07: the model rewrote the file
		// after the check, its output showed the fault, and it ended anyway).
		if change = h.idle.change(); change == "" {
			return ""
		}
	}
	note := h.idleNote(rounds)
	if note == "" {
		return ""
	}
	h.idle.mark()
	if change != "" {
		note = fmt.Sprintf(idleRearm, change) + "\n\n" + note
	}
	return note
}

// idleNote is the check itself: the requirements (--non-interactive, a turn
// that did work) and the main agent's open plan nodes.
func (h *Harness) idleNote(rounds int) string {
	var note string
	if h.cfg.NonInteractive && rounds > 1 {
		note = idleChecklist
	}
	if !h.isChild() {
		if open := h.idleOpenNodes(); open != "" {
			if note != "" {
				note += "\n\n"
			}
			note += fmt.Sprintf(idlePlanOpen, open)
		}
	}
	return note
}

// idleState is what the turn changed since its last check; prepareTurn
// resets it.
type idleState struct {
	mu      sync.Mutex
	checked bool
	paths   []string // file_edit and file_write paths, in order, once each
	ran     bool     // an exec
}

func (s *idleState) reset() {
	s.mu.Lock()
	s.checked, s.paths, s.ran = false, nil, false
	s.mu.Unlock()
}

// mark records a check: changes count from here.
func (s *idleState) mark() {
	s.mu.Lock()
	s.checked, s.paths, s.ran = true, nil, false
	s.mu.Unlock()
}

// note records one tool call, if a check came before it.
func (s *idleState) note(tool, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		return
	}
	switch tool {
	case "exec":
		s.ran = true
	case "file_edit", "file_write":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal([]byte(args), &a)
		if p := strings.TrimSpace(a.Path); p != "" && !slices.Contains(s.paths, p) {
			s.paths = append(s.paths, p)
		}
	}
}

// idlePathsMax is how many changed paths the second check names.
const idlePathsMax = 5

// change is the change since the check in words ("changed /a and ran
// commands"), "" with none.
func (s *idleState) change() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	if n := len(s.paths); n > 0 {
		ps := s.paths
		if n > idlePathsMax {
			ps = ps[:idlePathsMax]
		}
		what := "changed " + strings.Join(ps, ", ")
		if n > idlePathsMax {
			what += fmt.Sprintf(" and %d more files", n-idlePathsMax)
		}
		parts = append(parts, what)
	}
	if s.ran {
		parts = append(parts, "ran commands")
	}
	return strings.Join(parts, " and ")
}

// withIdleRearm records the calls that re-arm the check.
func withIdleRearm(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		h.idle.note(tc.Function.Name, tc.Function.Arguments)
		return inner(ctx, tc)
	}
}
