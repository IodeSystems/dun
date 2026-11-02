package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/taskgraph"
)

// The task plan (docs/taskplan.md): the graph and its rules are
// internal/taskgraph; this file is what a session does with it.
//
//   - Stored in <session>.plan.json beside the session, so a plan survives a
//     resume and a fold; a session with no file keeps it in memory.
//     Read on first use, and again if the session's file changed (as the
//     worklog is, worklog.go).
//   - The `plan` tool, in the built-in group "plan". Root sessions only:
//     a child gets no plan tool yet (delegation, a later step).
//   - A turn that begins while the plan has open nodes carries a short line
//     (planTurnLine).
//   - Plan mode (/plan mode on), and the person's approval. Both are the
//     host's, not the graph's: the graph knows nothing of them.

// PlanFile is the session's plan sidecar.
func PlanFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".plan.json"
}

// planFileData is the sidecar's content: the graph and the host's two flags.
type planFileData struct {
	Graph    taskgraph.Graph `json:"graph"`
	Mode     bool            `json:"mode,omitempty"`
	Approved bool            `json:"approved,omitempty"`
}

// planState is the session's plan in memory.
type planState struct {
	store *taskgraph.MemStore
	file  string // the session file it was read for
	// mode and approved: plan mode, and whether the person approved the
	// plan. Guarded by Harness.planMu.
	mode, approved bool
}

// planNow is the session's plan, read from its sidecar on first use.
func (h *Harness) planNow() *planState {
	h.planMu.Lock()
	defer h.planMu.Unlock()
	if h.plan != nil && h.plan.file == h.cfg.SessionFile {
		return h.plan
	}
	p := &planState{file: h.cfg.SessionFile}
	var g taskgraph.Graph
	if p.file != "" {
		if b, err := os.ReadFile(PlanFile(p.file)); err == nil {
			var d planFileData
			if json.Unmarshal(b, &d) == nil {
				g, p.mode, p.approved = d.Graph, d.Mode, d.Approved
			}
		}
	}
	p.store = taskgraph.NewMemStore(g, func(g taskgraph.Graph) error { return h.savePlan(p, g) })
	h.plan = p
	return p
}

// savePlan writes the sidecar; a session in memory keeps the plan there.
func (h *Harness) savePlan(p *planState, g taskgraph.Graph) error {
	if p.file == "" {
		return nil
	}
	h.planMu.Lock()
	d := planFileData{Graph: g, Mode: p.mode, Approved: p.approved}
	h.planMu.Unlock()
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(PlanFile(p.file), append(b, '\n'), 0o600)
}

// planStore is the tool's Store: the session's plan, wherever the session
// file now is, and a word to the UI after every change.
type planStore struct{ h *Harness }

func (s planStore) View(ctx context.Context) (taskgraph.Graph, error) {
	return s.h.planNow().store.View(ctx)
}

func (s planStore) Update(ctx context.Context, fn func(*taskgraph.Graph) error) error {
	if err := s.h.planNow().store.Update(ctx, fn); err != nil {
		return err
	}
	s.h.planChanged()
	return nil
}

// planChanged tells the UI the plan is different.
func (h *Harness) planChanged() {
	if h.cfg.OnPlan != nil {
		h.cfg.OnPlan()
	}
}

// planNote is the host's words in the tool's description: the graph's own
// say nothing of plan mode or of files.
const planNote = "A node may list files, the paths it expects to change. While plan mode is on, the user " +
	"must approve the plan before you change files or run commands: make the nodes, then ask them to " +
	"approve it (/plan approve); until then reading, searching and this tool work."

// withPlan adds the plan tool's handler to the dispatcher.
func withPlan(inner agent.ToolDispatcher, h *Harness, handle func(context.Context, string) string, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != taskgraph.ToolName {
			return inner(ctx, tc)
		}
		out := handle(ctx, tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall(taskgraph.ToolName, a, out)
		}
		return out, nil
	}
}

// planToolDef is the plan tool for a root session and its handler.
func (h *Harness) planTool() (llm.ToolDef, func(context.Context, string) string) {
	return taskgraph.Tool(planStore{h}, "", taskgraph.WithNote(planNote))
}

// ── the turn's line ─────────────────────────────────────────────────────

// planTurnLine puts the plan's brief in the conversation as the next turn
// begins, when the plan has open nodes. It is an aside, so it rides that turn
// at the tail of the conversation and never disturbs the cached prefix, and
// it is read fresh after a fold.
//
// WHEN, because a line per tool result would cost far more than it says: once
// at the start of each turn (user message, wake, loop run). Within a turn the
// model's own plan calls already answer with the node touched and what is
// ready, and a change the person makes mid-turn arrives as its own note
// (planTold). So it is at most five short lines per turn and none while
// nothing is open.
func (h *Harness) planTurnLine() {
	if h.isChild() {
		return
	}
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return
	}
	if line := g.Brief(""); line != "" {
		h.Aside("[plan] " + line)
	}
}

// ── plan mode ───────────────────────────────────────────────────────────

// planLocked reports whether plan mode refuses workspace changes now.
func (h *Harness) planLocked() bool {
	p := h.planNow()
	h.planMu.Lock()
	defer h.planMu.Unlock()
	return p.mode && !p.approved
}

// writeVerb finds an MCP tool's name saying it changes something. A guess:
// MCP tools do not say whether they are read-only.
var writeVerb = regexp.MustCompile(`(?i)(^|_|-)(write|edit|delete|remove|rename|move|create|apply|commit|push|exec|run|replace|insert|patch|install|kill|format|fix|refactor)(_|-|$)`)

// changesWorkspace reports whether a tool changes the workspace or reaches
// beyond the session, which plan mode refuses until the plan is approved.
// server is the MCP server a tool is from, "" for dun's own.
//
// Refused: exec (a command cannot be told read-only), the file tools that
// write (edit, write, delete, move), agent (a sub-agent has them all), mount,
// loop_ctl (schedules runs of its own), dun_msg (acts in another session),
// every tool of the shell server, and an MCP tool whose name has a write verb.
// Allowed: file read, grep and list, plan, ask_user, session, read_saved,
// recap, skill, memory, jobs, tell_parent, dun_list, and the other MCP tools.
func changesWorkspace(name, server string) bool {
	switch name {
	case "exec", "agent", "mount", "loop_ctl", "dun_msg":
		return true
	case "file_edit", "file_write", "file_delete", "file_move":
		return true
	}
	if server == "" {
		return false
	}
	if server == ServerShell {
		return true
	}
	bare := name
	if i := strings.LastIndex(name, "__"); i >= 0 {
		bare = name[i+2:]
	}
	return writeVerb.MatchString(bare)
}

// withPlanGate refuses the calls changesWorkspace names while plan mode is
// on and the plan is not approved. The refusal is a result the model reads,
// and says what to do.
func withPlanGate(inner agent.ToolDispatcher, h *Harness, serverOf map[string]string) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if n := tc.Function.Name; changesWorkspace(n, serverOf[n]) && h.planLocked() {
			return "ERROR: plan mode is on and the plan is not approved, so " + n + " is refused: it changes things. " +
				"Write the plan with the plan tool (nodes with done_when), tell the user, and wait for them to run /plan approve. " +
				"Reading, searching and the plan tool work meanwhile.", nil
		}
		return inner(ctx, tc)
	}
}

// ── the person's side ───────────────────────────────────────────────────

// PlanRow is one row of the plan as a tree, for a screen.
type PlanRow struct {
	ID, Title, Goal, Status, DoneWhen, Owner, Why, Account, Notes, Replaces string
	Needs, Files, Waiting                                                   []string
	Depth                                                                   int
	// Repeat: the node shows under another of its parents as well; this row
	// only points at it.
	Repeat bool
	// Ready: it can start now (the graph's own answer).
	Ready bool
}

// PlanSnapshot is the plan for a screen.
type PlanSnapshot struct {
	Mode, Approved bool
	Rows           []PlanRow
	Total, Done    int
}

// Plan is the session's plan as the tree a person reads.
func (h *Harness) Plan() PlanSnapshot {
	p := h.planNow()
	g, _ := p.store.View(context.Background())
	h.planMu.Lock()
	s := PlanSnapshot{Mode: p.mode, Approved: p.approved, Total: len(g.Nodes)}
	h.planMu.Unlock()
	for _, n := range g.Nodes {
		if n.Status == taskgraph.Done {
			s.Done++
		}
	}
	for _, r := range g.Tree() {
		n := r.Node
		ready, _ := g.CanStart(n.ID)
		row := PlanRow{ID: n.ID, Title: n.Title, Goal: n.Goal, Status: string(n.Status), DoneWhen: n.DoneWhen, Owner: n.Owner,
			Why: n.Why, Account: n.Account, Notes: n.Notes, Replaces: n.Replaces, Needs: n.Needs, Files: n.Files,
			Depth: r.Depth, Repeat: r.Repeat, Ready: ready}
		if n.Status == taskgraph.Todo {
			row.Waiting = g.Waiting(n.ID)
		}
		s.Rows = append(s.Rows, row)
	}
	return s
}

// PlanCmd runs a /plan action other than the screen: "" (the plan as text),
// "mode [on|off]", "approve", "edit <id> title|goal <text>", "drop <id>
// [why]". What the person changes reaches the model as a note: the approval
// wakes it (it was told to wait), the rest ride its next turn.
func (h *Harness) PlanCmd(ctx context.Context, action string) (string, error) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(action), " ")
	rest = strings.TrimSpace(rest)
	p := h.planNow()
	switch strings.ToLower(verb) {
	case "", "view":
		g, _ := p.store.View(ctx)
		return g.List() + h.planModeSuffix(p), nil
	case "mode":
		switch strings.ToLower(rest) {
		case "":
			return "plan mode: " + h.planModeWord(p), nil
		case "on", "off":
			on := strings.EqualFold(rest, "on")
			h.planMu.Lock()
			if on && !p.mode {
				p.approved = false
			}
			p.mode = on
			h.planMu.Unlock()
			if err := p.store.Update(ctx, func(*taskgraph.Graph) error { return nil }); err != nil { // writes the flags
				return "", err
			}
			h.planChanged()
			if on {
				h.Aside("[control/plan] The user turned plan mode on: until they approve the plan, tools that change files or run commands are refused. Write the plan with the plan tool, then wait for /plan approve.")
				return "plan mode: on · the model plans first; /plan approve lets it change things", nil
			}
			h.Aside("[control/plan] The user turned plan mode off: no approval is needed.")
			return "plan mode: off", nil
		}
		return "", fmt.Errorf("usage: /plan mode on|off")
	case "approve":
		g, _ := p.store.View(ctx)
		if len(g.Nodes) == 0 {
			return "", fmt.Errorf("the plan is empty: nothing to approve")
		}
		h.planMu.Lock()
		p.approved = true
		h.planMu.Unlock()
		if err := p.store.Update(ctx, func(*taskgraph.Graph) error { return nil }); err != nil {
			return "", err
		}
		h.planChanged()
		h.Notify("[control/plan] The user approved the plan. You may change files and run commands now; work the ready nodes.")
		return "plan approved", nil
	case "edit":
		id, tail, _ := strings.Cut(rest, " ")
		field, text, _ := strings.Cut(strings.TrimSpace(tail), " ")
		text = strings.TrimSpace(text)
		if id == "" || text == "" || (field != "title" && field != "goal") {
			return "", fmt.Errorf("usage: /plan edit <id> title|goal <text>")
		}
		patch := taskgraph.Patch{}
		if field == "title" {
			patch.Title = &text
		} else {
			patch.Goal = &text
		}
		if err := p.store.Update(ctx, func(g *taskgraph.Graph) error { return g.Update(id, patch) }); err != nil {
			return "", err
		}
		h.planChanged()
		h.Aside(fmt.Sprintf("[control/plan] The user changed node %s's %s to: %s", id, field, text))
		return fmt.Sprintf("node %s: %s set", id, field), nil
	case "drop":
		id, why, _ := strings.Cut(rest, " ")
		if id == "" {
			return "", fmt.Errorf("usage: /plan drop <id> [why]")
		}
		if why = strings.TrimSpace(why); why == "" {
			why = "dropped by the user"
		}
		var waiting int
		if err := p.store.Update(ctx, func(g *taskgraph.Graph) error {
			if err := g.SetStatus(id, taskgraph.Dropped, why); err != nil {
				return err
			}
			waiting = len(g.Dependents(id))
			return nil
		}); err != nil {
			return "", err
		}
		h.planChanged()
		note := "[control/plan] The user dropped node " + id + " (" + why + ")."
		if waiting > 0 {
			note += " " + strconv.Itoa(waiting) + " node(s) wait on it: replace it (a node with replaces " + id + ") and update their needs."
		}
		h.Aside(note)
		return "node " + id + " dropped", nil
	}
	return "", fmt.Errorf("usage: /plan [mode on|off | approve | edit <id> title|goal <text> | drop <id> [why]]")
}

func (h *Harness) planModeWord(p *planState) string {
	h.planMu.Lock()
	defer h.planMu.Unlock()
	switch {
	case !p.mode:
		return "off"
	case p.approved:
		return "on, approved"
	}
	return "on, not approved"
}

func (h *Harness) planModeSuffix(p *planState) string {
	if w := h.planModeWord(p); w != "off" {
		return "\nplan mode: " + w
	}
	return ""
}
