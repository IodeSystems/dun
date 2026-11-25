package acp

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// Stop conditions (docs/acp.md, "Config options"; dun's /until,
// harness/until.go). A select holds one value and dun's conditions are a
// set, so two options: mode (off, goal, plan, goal+plan) and turn_cap (off
// or a number of turns). A turn cap with mode off is "run N turns". The
// engine owns the conditions; the bridge sends /until lines and reads the
// until event. Two things are the bridge's own:
//
//   - A goal's text is the next prompt's (or the running prompt's, when
//     one is running as goal is chosen): until it comes the goal is
//     pending here, and the prompt sends the /until line before itself.
//   - A prompt is answered when its run stops, not at its first turn's
//     end: a done while the run goes on is held (untilHeld).

const (
	optMode    = "mode"
	optTurnCap = "turn_cap"
)

var (
	modeValues = []string{"off", "goal", "plan", "goal+plan"}
	capValues  = []string{"off", "5", "10", "20", "50"}
)

// untilWant is what the two options ask for.
type untilWant struct {
	goal, plan bool
	cap        int // 0: no turns condition
}

// pendingGoal is a goal chosen in the editor whose text the next prompt
// gives.
type pendingGoal struct {
	plan bool
	cap  int
}

// untilNow is the conditions the options show: the pending goal, else the
// engine's.
func (c config) untilNow() untilWant {
	if p := c.goalPending; p != nil {
		return untilWant{goal: true, plan: p.plan, cap: p.cap}
	}
	var w untilWant
	if c.until == nil {
		return w
	}
	for _, k := range c.until.Conds {
		switch k.Kind {
		case proto.UntilGoal:
			w.goal = true
		case proto.UntilPlan:
			w.plan = true
		case proto.UntilTurns:
			w.cap = k.N
		}
	}
	return w
}

func (w untilWant) mode() string {
	switch {
	case w.goal && w.plan:
		return "goal+plan"
	case w.goal:
		return "goal"
	case w.plan:
		return "plan"
	}
	return "off"
}

func selectOf(values []string, names map[string]string) sdk.SessionConfigSelectOptions {
	var opts sdk.SessionConfigSelectOptionsUngrouped
	for _, v := range values {
		o := sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(v), Name: v}
		if d := names[v]; d != "" {
			o.Description = sdk.Ptr(d)
		}
		opts = append(opts, o)
	}
	return sdk.SessionConfigSelectOptions{Ungrouped: &opts}
}

// modeOption is the stop condition: off, goal, plan, goal+plan.
func (c config) modeOption() sdk.SessionConfigOption {
	return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: optMode, Name: "Run until", Category: sdk.Ptr(sdk.SessionConfigOptionCategoryMode),
		Description: sdk.Ptr("When dun stops starting turns itself (dun's /until). goal: the next prompt is the goal, judged after each turn; " +
			"plan: until no plan node is ready. Without a turn cap, goal and plan stop at 20 turns."),
		CurrentValue: sdk.SessionConfigValueId(c.untilNow().mode()),
		Options: selectOf(modeValues, map[string]string{"off": "one turn per prompt (or the turn cap's turns)",
			"goal": "until the goal (the next prompt) is met", "plan": "until the plan is drained", "goal+plan": "until either"}),
	}}
}

// capOption is the turn cap: off, or N turns. A cap the engine has that is
// not in the list (a TUI's /until turns 15) is listed too.
func (c config) capOption() (sdk.SessionConfigOption, []string) {
	values := slices.Clone(capValues)
	current := "off"
	if n := c.untilNow().cap; n > 0 {
		current = strconv.Itoa(n)
		if !slices.Contains(values, current) {
			values = append(values, current)
		}
	}
	return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: optTurnCap, Name: "Turn cap", Category: sdk.Ptr(sdk.SessionConfigOptionCategory("_turn_cap")),
		Description:  sdk.Ptr("At most this many turns per prompt (dun's /until turns N); with Run until off, dun runs this many turns."),
		CurrentValue: sdk.SessionConfigValueId(current), Options: selectOf(values, map[string]string{"off": "no cap of its own (goal and plan: 20)"}),
	}}, values
}

// untilLine is the /until line for w; goal is the goal's text (and check)
// when w has one.
func untilLine(w untilWant, goal proto.UntilCond) string {
	var conds []proto.UntilCond
	if w.goal {
		conds = append(conds, goal)
	}
	if w.plan {
		conds = append(conds, proto.UntilCond{Kind: proto.UntilPlan})
	}
	if w.cap > 0 {
		conds = append(conds, proto.UntilCond{Kind: proto.UntilTurns, N: w.cap})
	}
	if len(conds) == 0 {
		return "off"
	}
	return proto.UntilLine(conds)
}

// untilGoal is the engine's goal condition, the zero value with none.
func (c config) untilGoal() proto.UntilCond {
	if c.until != nil {
		for _, k := range c.until.Conds {
			if k.Kind == proto.UntilGoal {
				return proto.UntilCond{Kind: proto.UntilGoal, Goal: k.Goal, Check: k.Check}
			}
		}
	}
	return proto.UntilCond{}
}

// setUntil is set_config_option for mode and turn_cap: the /until line to
// send and what the engine reports once it took it; cmd nil when there is
// nothing to send (the value is in force, or a goal waits for its prompt).
func (s *session) setUntil(id, value string) (cmd any, done func(config) bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	w := c.untilNow()
	switch id {
	case optMode:
		if !slices.Contains(modeValues, value) {
			return nil, nil, invalidValue(id, value, "not one of the mode option's values")
		}
		w.goal, w.plan = strings.Contains(value, "goal"), strings.Contains(value, "plan")
	case optTurnCap:
		_, values := c.capOption()
		if !slices.Contains(values, value) {
			return nil, nil, invalidValue(id, value, "not one of the turn cap option's values")
		}
		w.cap, _ = strconv.Atoi(value) // off: 0
	}
	if w == c.untilNow() {
		return nil, nil, nil
	}
	goal := c.untilGoal()
	if w.goal && goal.Goal == "" {
		// The prompt running gives the goal; else the next one does.
		for _, p := range s.prompts {
			if p.inTurn && p.text != "" {
				goal = proto.UntilCond{Kind: proto.UntilGoal, Goal: p.text}
				break
			}
		}
		if goal.Goal == "" {
			s.cfg.goalPending = &pendingGoal{plan: w.plan, cap: w.cap}
			return nil, nil, nil
		}
	}
	s.cfg.goalPending = nil
	want := w
	return s.untilCmd(untilLine(w, goal)), func(c config) bool { return c.goalPending == nil && c.untilNow() == want }, nil
}

// untilCmd is the control that sets a /until line; its reply is not shown
// (untilSent). s.mu is held.
func (s *session) untilCmd(line string) *proto.ControlCmd {
	s.untilSent++
	return &proto.ControlCmd{ID: "until", Action: line}
}

// goalPrompt is the /until line a prompt sends before itself when a goal
// waits for it, "" when none does. s.mu is held.
func (s *session) goalPrompt(text string) *proto.ControlCmd {
	p := s.cfg.goalPending
	if p == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	s.cfg.goalPending = nil
	return s.untilCmd(untilLine(untilWant{goal: true, plan: p.plan, cap: p.cap}, proto.UntilCond{Kind: proto.UntilGoal, Goal: text}))
}

// onUntil: the engine's conditions or run changed. A run's next turn is
// a line of the agent's text; a run that stopped says why, and answers the
// prompts held for it.
func (s *session) onUntil(u *proto.Until) {
	s.mu.Lock()
	prev := s.cfg.until
	held := s.untilHeld
	cancelled := s.cancel
	s.mu.Unlock()
	s.onConfig(func(c *config) { c.until = u })
	wasRunning := prev != nil && prev.Running
	switch {
	case u.Running && u.Turn > 1 && (prev == nil || prev.Turn != u.Turn):
		s.text(fmt.Sprintf("\n\n_until: turn %d of %d_\n\n", u.Turn, u.Cap))
	case !u.Running && wasRunning && u.Stopped != "":
		s.text("\n\n_until: stopped · " + u.Stopped + "_\n")
	}
	if !u.Running && held {
		s.mu.Lock()
		s.untilHeld = false
		s.mu.Unlock()
		stop := sdk.StopReasonEndTurn
		if cancelled {
			stop = sdk.StopReasonCancelled
		}
		s.endTurn(stop, nil)
	}
}

// ownUntilReply reports whether a /until reply answers a line the bridge
// sent (not shown, and no prompt's), using it up.
func (s *session) ownUntilReply() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.untilSent == 0 {
		return false
	}
	s.untilSent--
	return true
}

// untilRunning reports whether a run goes on past the turn that ended.
// s.mu is held.
func (s *session) untilRunning() bool { return s.cfg.until != nil && s.cfg.until.Running }
