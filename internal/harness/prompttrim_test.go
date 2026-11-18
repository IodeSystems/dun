package harness

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
)

type stubPeers struct{}

func (stubPeers) List(context.Context) ([]PeerSession, error) { return nil, nil }
func (stubPeers) Send(context.Context, string, string, bool) (PeerSent, error) {
	return PeerSent{}, nil
}
func (stubPeers) LoopCtl(context.Context, time.Duration, bool, string) (LoopState, error) {
	return LoopState{}, nil
}

func sessionHas(h *Harness, name string) bool {
	for _, n := range toolNamesOf(h) {
		if n == name {
			return true
		}
	}
	return false
}

// A worker (dun_start's session) is read by a model: no thinking headings, no
// reply formatting, no peer or loop tools. ask_user stays. A root has all.
func TestWorker_PromptAndToolSet(t *testing.T) {
	start := func(worker bool) *Harness {
		t.Setenv("DUN_HOME", t.TempDir())
		dir := t.TempDir()
		h, err := Start(context.Background(), Config{
			Workspace: dir, Servers: []mcpserver.Server{}, Client: &scriptRunner{}, Worker: worker,
			Peers: stubPeers{}, Loops: stubPeers{},
			Ask: func(context.Context, string, []string, bool, []string) (string, error) { return "", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		return h
	}
	root, w := start(false), start(true)
	for _, want := range []string{"# Thinking:", "Your replies are shown as Markdown"} {
		if !strings.Contains(root.Session.System, want) {
			t.Errorf("a root prompt lacks %q", want)
		}
		if strings.Contains(w.Session.System, want) {
			t.Errorf("a worker prompt has %q", want)
		}
	}
	for _, name := range []string{"dun_list", "dun_msg", "loop_ctl"} {
		if !sessionHas(root, name) {
			t.Errorf("a root lacks %s", name)
		}
		if sessionHas(w, name) {
			t.Errorf("a worker has %s", name)
		}
	}
	if !sessionHas(w, "ask_user") {
		t.Error("a worker lacks ask_user")
	}
}

// A child lacks the memory tool and the formatting and thinking text; its plan_add
// has childPlanNote and its plan_show does not.
func TestChild_PromptAndTools(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: &scriptRunner{}, Rag: &fakeRag{},
		ClientFor:   func(string, string) (ChildClient, error) { return ChildClient{Runner: &scriptRunner{}}, nil },
		SessionFile: dir + "/s.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if !sessionHas(h, "memory") {
		t.Fatal("the root lacks memory (the test premise)")
	}
	planCall(t, h, "plan_add", `{"title":"scan","done_when":"x"}`)
	callTool(t, h, "agent", map[string]any{"node": planIDs(h)["scan"]})
	sa := h.agentList()[0]
	waitIdle(t, sa)
	if sessionHas(sa.h, "memory") {
		t.Error("a child has the memory tool")
	}
	for _, not := range []string{"# Thinking:", "Your replies are shown as Markdown"} {
		if strings.Contains(sa.h.Session.System, not) {
			t.Errorf("a child prompt has %q", not)
		}
	}
	for _, d := range sa.h.Session.Tools {
		switch d.Function.Name {
		case "plan_add":
			if !strings.Contains(d.Function.Description, childPlanNote) {
				t.Error("plan_add lacks the child note")
			}
		case "plan_show":
			if strings.Contains(d.Function.Description, childPlanNote) {
				t.Error("plan_show has the child note")
			}
		}
	}
}

func TestExecToolDef_PersonNote(t *testing.T) {
	if d := execToolDef(true).Function.Description; !strings.Contains(d, "If the user writes while you wait") {
		t.Errorf("with a person: %s", d)
	}
	d := execToolDef(false).Function.Description
	if strings.Contains(d, "user") || !strings.Contains(d, "do not sleep or poll") || !strings.Contains(d, "sub-agent finishes while you wait") {
		t.Errorf("without a person: %s", d)
	}
}

func TestSkillsUsable(t *testing.T) {
	sk := []Skill{
		{Name: "plain", Description: "no tools named"},
		{Name: "dotted", Description: "uses mcp__claude_ai_Docs__create"},
		{Name: "hosted", Description: "uses mcp__corp-wiki__search"},
		{Name: "gone", Description: "uses mcp__claude-in-chrome__* and mcp__remote-devices__computer_*"},
	}
	tools := []mcpmgr.MCPTool{{Name: "create", ServerID: "claude.ai Docs"}}
	var got []string
	for _, s := range skillsUsable(sk, tools, "corp_wiki") {
		got = append(got, s.Name)
	}
	if strings.Join(got, " ") != "plain dotted hosted" {
		t.Errorf("kept %v", got)
	}
}

func TestSkillDesc_CutsByRuneAtAWord(t *testing.T) {
	if d := "short one"; skillDesc(d) != d {
		t.Error("a short description changed")
	}
	long := strings.Repeat("héllo wörld ", 30)
	got := skillDesc(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) > skillDescMax {
		t.Errorf("cut: %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	if body := strings.TrimSuffix(got, "…"); !strings.HasSuffix(body, "wörld") && !strings.HasSuffix(body, "héllo") {
		t.Errorf("not cut at a word: %q", got)
	}
	if got := skillDesc(strings.Repeat("é", 300)); utf8.RuneCountInString(got) != skillDescMax {
		t.Errorf("no space to cut at: %d runes", utf8.RuneCountInString(got))
	}
}

// The child's answer is what it published on its node: the account of a done
// node, the reason of a dropped one; it wins over the prose the turn ended
// with. A child that publishes nothing answers with its reply.
func TestAgentAnswer_PlanAccountWinsOverProse(t *testing.T) {
	spawn := func(title string) (*Harness, *subAgent, string) {
		h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("the prose"), textRound("the prose")}})
		planCall(t, h, "plan_add", `{"title":"`+title+`","done_when":"x"}`)
		id := planIDs(h)[title]
		callTool(t, h, "agent", map[string]any{"node": id, "model": "m"})
		sa := h.agentList()[0]
		waitIdle(t, sa)
		return h, sa, id
	}
	_, sa, id := spawn("a")
	if r := sa.report(); !strings.Contains(r, "answer: the prose the prose") {
		t.Errorf("fallback to the reply: %s", r)
	}
	_, sa, id = spawn("b")
	closeNode(t, sa, `{"outcome":"done","id":"`+id+`","account":"found it at main.go:12"}`)
	if r := sa.report(); !strings.Contains(r, "answer: node "+id+" done: found it at main.go:12") || strings.Contains(r, "the prose") {
		t.Errorf("account: %s", r)
	}
	_, sa, id = spawn("c")
	closeNode(t, sa, `{"outcome":"drop","id":"`+id+`","reason":"missing tools: file_read"}`)
	if r := sa.report(); !strings.Contains(r, "node "+id+" dropped: missing tools: file_read") || strings.Contains(r, "the prose") {
		t.Errorf("drop: %s", r)
	}
}

func closeNode(t *testing.T, sa *subAgent, args string) {
	t.Helper()
	id := args[strings.Index(args, `"id":"`)+6:]
	id = id[:strings.Index(id, `"`)]
	sa.harness().childPlanTool().Dispatch(context.Background(), "plan_update", `{"id":"`+id+`","status":"doing"}`)
	if out := sa.harness().childPlanTool().Dispatch(context.Background(), "plan_close", args); strings.Contains(out, "ERROR") {
		t.Fatalf("plan_close: %q", out)
	}
}
