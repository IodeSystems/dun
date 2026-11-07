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
//   - The `plan` tool, in the built-in group "plan". A root's covers the
//     graph; a child gets one only while it owns nodes (childPlanTool).
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
		taskgraph.WithOnChange(func(_ context.Context, _ string, cs []taskgraph.Change) {
			h.planTell(cs, "the main agent")
		}))
}

// planTell routes a kept change to the owners it names, without the actor
// (taskgraph.Change.Tell); who is the actor, in words. Owner "" is this
// session's main agent: it hears the board as it stands now, ONE aside keyed
// "plan" that every change replaces, never a pile of change notes
// (docs/attention.md, "The plan board"). A child that owns a changed node
// hears which nodes and who changed them, inside its running turn or at its
// next one.
func (h *Harness) planTell(cs []taskgraph.Change, who string) {
	main, kids := false, map[int][]string{}
	for _, c := range cs {
		for _, o := range c.Tell {
			if o == "" {
				main = true
			} else if k, ok := planAgentNum(o); ok && !slices.Contains(kids[k], c.ID) {
				kids[k] = append(kids[k], c.ID)
			}
		}
	}
	if main {
		h.planBoard()
	}
	for k, ids := range kids {
		if sa := h.agentByID(k); sa != nil {
			if child := sa.harness(); child != nil {
				child.Notify(fmt.Sprintf("[control/plan] %s changed plan node(s) %s, which you own. "+
					"plan(show) has them as they are now; carry on to match.", who, strings.Join(ids, ", ")))
			}
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
// planOpen is the plan's open work in a few short lines, "" with none: what
// a fold is told the session is for (fold.go).
func (h *Harness) planOpen() string {
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return ""
	}
	return g.Brief("")
}

// PlanBrief is a session's plan brief read from its sidecar, for dund's peer
// list (what another session is doing); "" with no plan.
func PlanBrief(sessionFile string) string {
	b, err := os.ReadFile(PlanFile(sessionFile))
	if err != nil {
		return ""
	}
	var d planFileData
	if json.Unmarshal(b, &d) != nil {
		return ""
	}
	return d.Graph.Brief("")
}

func (h *Harness) planTurnLine() {
	if h.isChild() {
		return
	}
	if line := h.planOpen(); line != "" {
		h.AsideWith("[plan] "+line, AsideOpts{Key: "plan"})
	}
}

// planBoard queues the current board for the main agent, replacing the one
// still queued. With nothing open it says so: a board that silently kept the
// earlier text would show nodes that are done.
func (h *Harness) planBoard() {
	line := h.planOpen()
	if line == "" {
		line = "no open items."
	}
	h.AsideWith("[plan] "+line, AsideOpts{Key: "plan"})
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
// Allowed: file read, grep and list, plan, ask_user,
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
				return "plan mode: on · the model plans first; /plan approve lets it change things", nil
			}
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
		h.planTell(cs, "the user")
		return fmt.Sprintf("node %s: %s set", id, field), nil
	case "drop":
		id, why, _ := strings.Cut(rest, " ")
		if id == "" {
			return "", fmt.Errorf("usage: /plan drop <id> [why]")
		}
		if why = strings.TrimSpace(why); why == "" {
			why = "dropped by the user"
		}
		cs, err := taskgraph.Apply(ctx, p.store, planActorUser, func(g *taskgraph.Graph) error {
			return g.SetStatus(id, taskgraph.Dropped, why)
		})
		if err != nil {
			return "", err
		}
		h.planChanged()
		h.planTell(cs, "the user")
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
// it); a child, inside its running turn (its run continues for it) or as its
// next run. A node whose child is gone, stopped or dismissed goes to the main
// agent: no one else can act on it.
func (h *Harness) planTellOwner(id, text string) (string, error) {
	g, _ := h.planNow().store.View(context.Background())
	n := g.Get(id)
	if n == nil {
		return "", fmt.Errorf("no plan node %s", id)
	}
	msg := fmt.Sprintf("[control/plan] The user, about plan node %s (%q): %s\n"+
		"Change the plan to match with the plan tool (this node, or the nodes under it), then carry on.", id, n.Title, text)
	k, isAgent := planAgentNum(n.Owner)
	var sa *subAgent
	var child *Harness
	if isAgent {
		if sa = h.agentByID(k); sa != nil {
			child = sa.harness()
		}
	}
	if child == nil {
		h.Notify(msg)
		if isAgent {
			return fmt.Sprintf("told the main agent about node %s (agent #%d, its owner, is not running)", id, k), nil
		}
		return fmt.Sprintf("told the main agent about node %s", id), nil
	}
	if state, _, _, _ := sa.snapshot(); state == agentRunning {
		child.Say(msg)
		h.Aside(fmt.Sprintf("The user told agent #%d about plan node %s: %q.", k, id, oneLine(text, 300)))
		return fmt.Sprintf("told agent #%d about node %s (it is working; it reads this in its turn)", k, id), nil
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

// The main agent hands out the plan breadth first (USER 2026-11-06): only a
// ready node — todo, nobody's, every need done — goes to a child, and only
// that node, to a new child (agent's node) or an idle one (jobs' node). So no
// child waits on another's node; when a child's nodes are done, the main
// agent gives what became ready to the same children or to new ones. A
// child may hold several. It owes a publish for each — done with notes of
// what it found or made, or dropped with why — and a run that ends owing is
// reminded once (subAgent.work). Nodes it adds are its own, and owed too.
// Owning takes a node out of the main agent's ready list; dismissing the
// child gives back what it did not finish.

// planOwner is a child as a node's owner: "agent:N".
func planOwner(num int) string { return "agent:" + strconv.Itoa(num) }

// planAgentNum is the child an owner names ("agent:N").
func planAgentNum(owner string) (int, bool) {
	n, ok := strings.CutPrefix(owner, "agent:")
	if !ok {
		return 0, false
	}
	k, err := strconv.Atoi(n)
	return k, err == nil && k > 0
}

// planCanAssign says why node id cannot go to a child, or nil.
func (h *Harness) planCanAssign(id string) error {
	if h.isChild() {
		return fmt.Errorf("a sub-agent has no plan of its own; only the main agent gives nodes to children")
	}
	g, _ := h.planNow().store.View(context.Background())
	n := g.Get(id)
	switch {
	case n == nil:
		return fmt.Errorf("no plan node %s", id)
	case n.Owner != "":
		return fmt.Errorf("node %s already belongs to %s", id, n.Owner)
	case n.Status != taskgraph.Todo:
		return fmt.Errorf("node %s is %s; only a todo node can be given", id, n.Status)
	}
	if ok, _ := g.CanStart(id); !ok {
		return fmt.Errorf("node %s is not ready: it needs %s first. Give its needs out, or do them, and give it when they are done",
			id, strings.Join(n.Needs, ", "))
	}
	return nil
}

// planAssign gives node id, alone, to child num, and the child its plan tool.
func (h *Harness) planAssign(id string, num int) error {
	if err := h.planCanAssign(id); err != nil {
		return err
	}
	if err := h.planNow().store.Update(context.Background(), func(g *taskgraph.Graph) error {
		n := g.Get(id)
		if n == nil || n.Owner != "" {
			return fmt.Errorf("node %s was taken meanwhile", id)
		}
		n.Owner = planOwner(num)
		return nil
	}); err != nil {
		return err
	}
	h.planChanged()
	if sa := h.agentByID(num); sa != nil {
		if child := sa.harness(); child != nil {
			child.applyTools() // the plan tool, now that it owns a node
		}
	}
	return nil
}

// giveNode gives ready node id to idle child num: what agent({id:N, node:})
// does. It returns the child's next message — the node's brief, then tell.
func (h *Harness) giveNode(num int, id, tell string) (string, error) {
	sa := h.agentByID(num)
	if sa == nil {
		return "", fmt.Errorf("no agent #%d", num)
	}
	switch state, _, _, _ := sa.snapshot(); {
	case state == agentRunning:
		return "", fmt.Errorf("agent #%d is still running; give it node %s once it reports, or start a new agent with agent(node:%q)", num, id, id)
	case sa.harness() == nil:
		return "", fmt.Errorf("agent #%d is %s; resume it first, or start a new agent with agent(node:%q)", num, state, id)
	}
	if err := h.planAssign(id, num); err != nil {
		return "", err
	}
	msg := "Work this plan node next." + h.planBrief(id)
	if tell = strings.TrimSpace(tell); tell != "" {
		msg += "\n\n" + tell
	}
	return msg, nil
}

// planBrief is what a child is told of a node it is given.
func (h *Harness) planBrief(id string) string {
	g, _ := h.planNow().store.View(context.Background())
	n := g.Get(id)
	if n == nil {
		return ""
	}
	b := fmt.Sprintf("\n\nYou own plan node %s: %s.", id, n.Title)
	if n.Goal != "" {
		b += " Goal: " + n.Goal + "."
	}
	if n.DoneWhen != "" {
		b += " Done when: " + n.DoneWhen + "."
	}
	return b + " Publish it with the plan tool when its work is done: status done with notes of what you " +
		"found or made, or dropped with why. The main agent plans what follows from what you publish."
}

// planRelease gives back to the main agent the nodes child num owned and did
// not finish, so they return to its ready list instead of waiting on a child
// that is gone. Finished nodes keep their owner: who did them.
func (h *Harness) planRelease(num int) {
	owner, released := planOwner(num), false
	_ = h.planNow().store.Update(context.Background(), func(g *taskgraph.Graph) error {
		for _, n := range g.Nodes {
			if n.Owner == owner && planOpen(n) {
				n.Owner, released = "", true
			}
		}
		return nil
	})
	if released {
		h.planChanged()
	}
}

// planReleaseOrphans gives back the unfinished nodes of children this session
// no longer has, or has dismissed (a resume after a crash that kept no record
// of one).
func (h *Harness) planReleaseOrphans() {
	g, _ := h.planNow().store.View(context.Background())
	seen := map[int]bool{}
	for _, n := range g.Nodes {
		k, ok := planAgentNum(n.Owner)
		if !ok || seen[k] || !planOpen(n) {
			continue
		}
		seen[k] = true
		if sa := h.agentByID(k); sa == nil || sa.isDismissed() {
			h.planRelease(k)
		}
	}
}

// planOpen: not done and not dropped.
func planOpen(n *taskgraph.Node) bool {
	return n.Status != taskgraph.Done && n.Status != taskgraph.Dropped
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

// planOwed is the nodes owner has not published yet.
func (h *Harness) planOwed(owner string) []*taskgraph.Node {
	g, _ := h.planNow().store.View(context.Background())
	var out []*taskgraph.Node
	for _, n := range g.Nodes {
		if n.Owner == owner && planOpen(n) {
			out = append(out, n)
		}
	}
	return out
}

// childPlanNote is the child's plan tool's extra sentence.
const childPlanNote = "This is your parent's plan. You own some of its nodes and may change, add under and " +
	"finish only those; the rest are shown so you can see what yours wait on and what waits on yours. " +
	"You owe a publish for each node you own: status done with notes of what you found or made, or dropped with why."

// childPlanTool is a child's plan tool: its parent's graph, as its owner
// ("agent:N"), changing only that owner's nodes. What it changes reaches the
// parent's screen at once; a publish (done or dropped) wakes the parent's
// main agent, with what became ready, and any other change rides its next turn.
func (h *Harness) childPlanTool() (llm.ToolDef, func(context.Context, string) string) {
	parent, actor := h.cfg.Parent, planOwner(h.cfg.AgentID)
	return taskgraph.Tool(planStore{parent}, actor, taskgraph.WithNote(childPlanNote),
		taskgraph.WithMayChange(func(a string, n *taskgraph.Node) bool { return n != nil && n.Owner == a }),
		taskgraph.WithOnChange(func(_ context.Context, _ string, cs []taskgraph.Change) {
			parent.planFromChild(h.cfg.AgentID, cs)
		}))
}

// planFromChild tells the main agent what child num changed.
func (h *Harness) planFromChild(num int, cs []taskgraph.Change) {
	var ids, published, ready []string
	for _, c := range cs {
		if !slices.Contains(ids, c.ID) {
			ids = append(ids, c.ID)
		}
		if c.After != nil && !planOpen(c.After) && (c.Before == nil || planOpen(c.Before)) {
			published = append(published, fmt.Sprintf("%s %s", c.ID, c.After.Status))
		}
		if c.Ready && c.After != nil && c.After.Owner == "" {
			ready = append(ready, c.ID)
		}
	}
	who := fmt.Sprintf("agent #%d", num)
	if len(published) == 0 {
		h.planTell(cs, who)
		return
	}
	msg := fmt.Sprintf("[control/plan] %s published plan node(s) %s (plan(show, id) has its notes).", who, strings.Join(published, ", "))
	if len(ready) > 0 {
		msg += " Now ready: " + strings.Join(ready, ", ") + " — give them to an idle agent (agent({id:N, node:…})), a new one (agent({prompt:…, node:…})), or do them."
	}
	h.planTell(cs, who)
	h.Notify(msg)
}
