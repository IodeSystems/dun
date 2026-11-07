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
// The wording keeps the turn open when a requirement fails: TB2.1
// winning-avg-corewars (2026-11-07) found 35% where 75% was required, wrote
// "I should attempt to improve stone before finishing — but I'm in an
// idle_check asking to verify and give final message", and stopped; pi kept
// working and passed.
const idleChecklist = "Before you end the turn: list each requirement the task states (file paths, names, formats, values, behaviors) and check each against what you produced now: run it where it can run, read it only where it cannot. If any is not met and you can still work on it, do not end the turn: keep working until it is met. End the turn only when every requirement is met, or when one cannot be met; then say which and why."

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
