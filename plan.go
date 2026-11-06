package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
	"github.com/iodesystems/dun/internal/fsutil"
)

// The task plan (docs/taskplan.md): the graph and its rules are agentkit's
// taskgraph; this file is what a session does with it, and the store.
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
	store *planMemStore
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
	p.store = &planMemStore{g: g, save: func(g taskgraph.Graph) error { return h.savePlan(p, g) }}
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

// planMemStore keeps a plan in memory, and hands it to save after each kept
// change (nil save: a session with no file). agentkit's taskgraph ships no
// store: the host provides it. One per session's plan, shared by every caller.
type planMemStore struct {
	mu sync.Mutex
	g  taskgraph.Graph
	// save writes the plan out. If it fails the change is undone.
	save func(g taskgraph.Graph) error
}

func (s *planMemStore) View(context.Context) (taskgraph.Graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.g.Clone(), nil
}

func (s *planMemStore) Update(_ context.Context, fn func(g *taskgraph.Graph) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.g.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if s.save != nil {
		if err := s.save(next); err != nil {
			return err
		}
	}
	s.g = next
	return nil
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
	return taskgraph.Tool(planStore{h}, "", taskgraph.WithNote(planNote),
		taskgraph.WithOnChange(func(_ context.Context, _ string, cs []taskgraph.Change) { h.planTell(cs, "") }))
}

// planTell routes a kept change to the owners it names, without the actor
// (taskgraph.Change.Tell). Owner "" is this session's main agent, told by an
// aside on its next turn; note is what it hears. Every node is the main
// agent's until delegation (docs/taskplan.md) gives a subtree another owner,
// and with it the session to inject into, so another owner hears nothing yet.
func (h *Harness) planTell(cs []taskgraph.Change, note string) {
	if note == "" {
		return
	}
	for _, c := range cs {
		if slices.Contains(c.Tell, "") {
			h.Aside(note)
			return
		}
	}
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
		cs, err := taskgraph.Apply(ctx, p.store, planActorUser, func(g *taskgraph.Graph) error { return g.Update(id, patch) })
		if err != nil {
			return "", err
		}
		h.planChanged()
		h.planTell(cs, fmt.Sprintf("[control/plan] The user changed node %s's %s to: %s", id, field, text))
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
		cs, err := taskgraph.Apply(ctx, p.store, planActorUser, func(g *taskgraph.Graph) error {
			if err := g.SetStatus(id, taskgraph.Dropped, why); err != nil {
				return err
			}
			waiting = len(g.Dependents(id))
			return nil
		})
		if err != nil {
			return "", err
		}
		h.planChanged()
		note := "[control/plan] The user dropped node " + id + " (" + why + ")."
		if waiting > 0 {
			note += " " + strconv.Itoa(waiting) + " node(s) wait on it: replace it (a node with replaces " + id + ") and update their needs."
		}
		h.planTell(cs, note)
		return "node " + id + " dropped", nil
	case "tell":
		id, text, _ := strings.Cut(rest, " ")
		if id == "" || strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("usage: /plan tell <id> <what to change>")
		}
		return h.planTellOwner(id, strings.TrimSpace(text))
	}
	return "", fmt.Errorf("usage: /plan [mode on|off | approve | edit <id> title|goal <text> | drop <id> [why] | tell <id> <text>]")
}

// planTellOwner hands what the person said about node id to whoever owns it,
// to change the plan to match — not a direct edit (USER 2026-11-05: "Decide
// x" nodes want "it's this way", and the owner should make the change). The
// main agent is told as /plan approve tells it (a notification, which wakes
// it); a child, inside its running turn or as its next run.
func (h *Harness) planTellOwner(id, text string) (string, error) {
	g, _ := h.planNow().store.View(context.Background())
	n := g.Get(id)
	if n == nil {
		return "", fmt.Errorf("no plan node %s", id)
	}
	msg := fmt.Sprintf("[control/plan] The user, about plan node %s (%q): %s\n"+
		"Change the plan to match with the plan tool (this node, or the nodes under it), then carry on.", id, n.Title, text)
	num, isAgent := strings.CutPrefix(n.Owner, "agent:")
	if !isAgent {
		h.Notify(msg)
		return fmt.Sprintf("told the main agent about node %s", id), nil
	}
	k, _ := strconv.Atoi(num)
	sa := h.agentByID(k)
	if sa == nil {
		return "", fmt.Errorf("node %s belongs to agent #%s, which is gone", id, num)
	}
	state, _, _, _ := sa.snapshot()
	sa.mu.Lock()
	child := sa.h
	sa.mu.Unlock()
	switch {
	case state == agentRunning && child != nil:
		child.Say(msg)
		h.Aside(fmt.Sprintf("The user told agent #%d about plan node %s: %q.", k, id, oneLine(text, 300)))
		return fmt.Sprintf("told agent #%d about node %s (it is working; it reads this in its turn)", k, id), nil
	case child == nil:
		return "", fmt.Errorf("agent #%d is %s; resume it before telling it something", k, state)
	}
	out := h.AgentTell(k, msg)
	if !strings.HasPrefix(out, "Told agent") {
		return "", fmt.Errorf("%s", out)
	}
	return fmt.Sprintf("told agent #%d about node %s", k, id), nil
}

// planActorUser is the person, as an actor in taskgraph.Apply: never an owner,
// so a change they make is told to every owner it touches.
const planActorUser = "user"

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

// ── nodes a child works ─────────────────────────────────────────────────

// A child spawned for a plan node (the agent tool's `node`) owns that node and
// everything that needs it, until it is dismissed (USER 2026-11-05: color the
// plan by agent, and dive from a node into the agent working it). Owning them
// takes them out of the parent's ready list, which keeps the parent from
// starting them too. The child gets a plan tool over its parent's graph that
// changes only its own nodes (childPlanTool), so the person can talk to the
// owner of a node and the owner changes it (PlanCmd "tell").

// planOwner is a child as a node's owner: "agent:N", as jobs(agent:N) names it.
func planOwner(num int) string { return "agent:" + strconv.Itoa(num) }

// planCanAssign says why node id cannot go to a new child, or nil.
func (h *Harness) planCanAssign(id string) error {
	if h.isChild() {
		return fmt.Errorf("a sub-agent has no plan; only the main agent gives nodes to children")
	}
	g, _ := h.planNow().store.View(context.Background())
	if g.Get(id) == nil {
		return fmt.Errorf("no plan node %s", id)
	}
	for _, n := range g.Subtree(id) {
		if n.Owner != "" {
			return fmt.Errorf("node %s already belongs to %s", n.ID, n.Owner)
		}
	}
	return nil
}

// planAssign gives node id, and what needs it, to child num.
func (h *Harness) planAssign(id string, num int) error {
	p := h.planNow()
	if err := p.store.Update(context.Background(), func(g *taskgraph.Graph) error {
		return g.Assign(id, planOwner(num))
	}); err != nil {
		return err
	}
	h.planChanged()
	return nil
}

// planRelease gives back to the main agent the nodes a dismissed child owned
// and did not finish, so they return to its ready list instead of waiting on
// a child that is gone. Finished nodes keep their owner: who did them.
func (h *Harness) planRelease(num int) {
	owner, released := planOwner(num), false
	p := h.planNow()
	_ = p.store.Update(context.Background(), func(g *taskgraph.Graph) error {
		for _, n := range g.Nodes {
			if n.Owner == owner && n.Status != taskgraph.Done && n.Status != taskgraph.Dropped {
				n.Owner, released = "", true
			}
		}
		return nil
	})
	if released {
		h.planChanged()
	}
}

// planOwns says whether owner owns any node of this session's plan.
func (h *Harness) planOwns(owner string) bool {
	g, _ := h.planNow().store.View(context.Background())
	for _, n := range g.Nodes {
		if n.Owner == owner {
			return true
		}
	}
	return false
}

// childPlanNote is the child's plan tool's extra sentence.
const childPlanNote = "This is your parent's plan. You own some of its nodes and may change, add under and " +
	"finish only those; the rest are shown so you can see what yours wait on and what waits on yours."

// childPlanTool is a child's plan tool: its parent's graph, as owner
// cfg.PlanOwner, changing only that owner's nodes. What it changes reaches the
// parent's screen at once, and the parent's main agent when it touches a
// node of theirs.
func (h *Harness) childPlanTool() (llm.ToolDef, func(context.Context, string) string) {
	parent, actor := h.cfg.Parent, h.cfg.PlanOwner
	return taskgraph.Tool(planStore{parent}, actor, taskgraph.WithNote(childPlanNote),
		taskgraph.WithMayChange(func(a string, n *taskgraph.Node) bool { return n != nil && n.Owner == a }),
		taskgraph.WithOnChange(func(_ context.Context, _ string, cs []taskgraph.Change) {
			ids := make([]string, 0, len(cs))
			for _, c := range cs {
				ids = append(ids, c.ID)
			}
			parent.planTell(cs, fmt.Sprintf("[control/plan] agent #%d changed plan node(s) %s.",
				h.cfg.AgentID, strings.Join(slices.Compact(ids), ", ")))
		}))
}
