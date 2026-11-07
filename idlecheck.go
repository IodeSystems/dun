package dun

import (
	"context"
	"fmt"
)

// idlecheck.go — what dun says when the model is about to end a turn
// (agent.Session.OnIdle). Asked at most once per turn.
//
// --non-interactive: no one reviews the result, so a turn that did work (more
// than one round) ends only after the model has checked the task's stated
// requirements against what it produced. Measured need: TB2.1 2026-11-07,
// nginx-request-logging put a directive in the wrong file the task named;
// the system prompt's finish-and-check line alone did not catch it.
//
// Interactive: the person is there and reads the final message, so no extra
// round, unless the plan still has open items: the model then continues them
// or says why they remain (USER 2026-11-07).

// idleChecklist is the --non-interactive check.
const idleChecklist = "Before you end the turn: list each requirement the task states (file paths, names, formats, values, behaviors). For each, say how you checked it against what you produced: a command you ran or a file you read, now. Fix anything that does not match. Then give your final message."

// idlePlanOpen is the note when the plan has open items.
const idlePlanOpen = "The plan still has open items:\n%s\nContinue them, or end the turn saying why each remains."

func (h *Harness) onIdle(_ context.Context, rounds, asks int) string {
	if asks > 0 {
		return ""
	}
	var note string
	if h.cfg.NonInteractive && rounds > 1 {
		note = idleChecklist
	}
	if !h.isChild() {
		if open := h.planOpen(); open != "" {
			if note != "" {
				note += "\n\n"
			}
			note += fmt.Sprintf(idlePlanOpen, open)
		}
	}
	return note
}
