package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// asker is an AskFunc that records its questions and answers with ans.
type asker struct {
	mu   sync.Mutex
	ans  string
	qs   []string
	opts [][]string
}

func (a *asker) ask(_ context.Context, q string, opts []string, _ bool, _ []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.qs, a.opts = append(a.qs, q), append(a.opts, opts)
	return a.ans, nil
}

func (a *asker) asked() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.qs)
}

// startNotProject starts a session whose root the fake poly-lsp refuses as
// not a project, with ask as the question the person answers.
func startNotProject(t *testing.T, ws string, ask AskFunc, env ...string) (*Harness, string, chan string) {
	t.Helper()
	lsp, log := fakeBin(t, "poly-lsp-mcp", "poly-lsp", append([]string{fakeIndexEnv + "=notproject"}, env...)...)
	msgs := make(chan string, 8)
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"), Ask: ask,
		Servers:       []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
		OnServerIndex: func(id, msg string) { msgs <- msg },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, log, msgs
}

// savedLocal is the project's saved server config, as text.
func savedLocal(ws string) string {
	b, _ := os.ReadFile(filepath.Join(ResolveProject(ws).WriteDir(), LocalServersFile))
	return string(b)
}

// A root that is not a project: dun asks once; "index anyway" is saved for
// the project, the server restarts with --index-anyway, a later session
// starts with the flag and asks nothing, and index-anyway off undoes it.
func TestIndexAnyway_AskedAndAccepted(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	a := &asker{ans: "index anyway"}
	h, log, msgs := startNotProject(t, ws, a.ask, fakeAnywayEnv+"=1")
	msg := nextMsg(t, msgs)
	wantQ := "dun is running in ~ — not a project (12 git repos inside). Index all of it anyway, or start dun in one of them? (the code tools need a project)\n" +
		"start dun in a repository instead: cd a && dun · cd b && dun · cd c && dun · cd d && dun · cd e && dun · and 7 more"
	if a.asked() != 1 || a.qs[0] != wantQ {
		t.Fatalf("asked %d: %q", a.asked(), a.qs)
	}
	if got := strings.Join(a.opts[0], "|"); got != "index anyway|not now" {
		t.Errorf("options %q", got)
	}
	for _, want := range []string{"poly-lsp: indexing ~ anyway, for this project (saved to ", "undo: /lsp index-anyway off"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the line lacks %q:\n%s", want, msg)
		}
	}
	if m := nextMsg(t, msgs); m != "" { // the restarted build is ready, quietly
		t.Errorf("the ready build said %q", m)
	}
	waitIndex(t, h, IndexReady)
	if st := codeState(h); !st.IndexAnyway || !h.IndexAnyway(ServerCode) {
		t.Fatalf("not set: %+v", st)
	}
	if args := argLines(log); len(args) != 2 || strings.Contains(args[0], "--index-anyway") || !strings.HasSuffix(args[1], "--index-anyway") {
		t.Fatalf("spawns: %q", args)
	}
	if !strings.Contains(savedLocal(ws), `"index_anyway": true`) {
		t.Fatalf("not saved for the project: %s", savedLocal(ws))
	}
	h.Close()

	// A later session: the saved yes is on the first spawn; nothing is asked.
	a2 := &asker{ans: "not now"}
	h2, log2, msgs2 := startNotProject(t, ws, a2.ask, fakeAnywayEnv+"=1")
	if m := nextMsg(t, msgs2); m != "" {
		t.Errorf("the later session said %q", m)
	}
	waitIndex(t, h2, IndexReady)
	if args := argLines(log2); len(args) != 1 || !strings.HasSuffix(args[0], "--index-anyway") || a2.asked() != 0 {
		t.Fatalf("later spawn: %q, asked %d", args, a2.asked())
	}

	// The undo restarts without the flag and takes the entry out.
	_, changed, err := h2.SetIndexAnyway(context.Background(), ServerCode, false)
	if err != nil || !changed || h2.IndexAnyway(ServerCode) {
		t.Fatalf("off: %v %v", changed, err)
	}
	if args := argLines(log2); len(args) != 2 || strings.Contains(args[1], "--index-anyway") {
		t.Fatalf("after off: %q", args)
	}
	if strings.Contains(savedLocal(ws), "index_anyway") {
		t.Errorf("still saved: %s", savedLocal(ws))
	}
	if _, changed, _ := h2.SetIndexAnyway(context.Background(), ServerCode, false); changed {
		t.Error("off twice changed something")
	}
}

// "not now": nothing is saved or restarted, the line names repositories to
// start dun in (five of them), and the question is not put a second time.
func TestIndexAnyway_NotNowAsksOnce(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	a := &asker{ans: "not now"}
	h, log, msgs := startNotProject(t, ws, a.ask, fakeAnywayEnv+"=1")
	msg := nextMsg(t, msgs)
	for _, want := range []string{"poly-lsp: off for this session — " + ws + " is not a project; the rest of dun works.",
		"cd a && dun · cd b && dun · cd c && dun · cd d && dun · cd e && dun · and 7 more",
		"/lsp index-anyway on indexes " + ws + " regardless."} {
		if !strings.Contains(msg, want) {
			t.Errorf("the line lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "cd f && dun") {
		t.Errorf("more than five repositories named:\n%s", msg)
	}
	if st := waitIndex(t, h, IndexFailed); st.IndexAnyway || !strings.HasPrefix(st.IndexErr, "not a project") {
		t.Fatalf("%+v", st)
	}
	// The same failure again (a manual restart): no second question.
	h.noteIndex(ServerCode, indexNote{State: IndexFailed, Reason: reasonNotProject, Error: "not a project: x", Repos: []string{"a"}})
	if m := nextMsg(t, msgs); m != "" {
		t.Errorf("asked again: %q", m)
	}
	if a.asked() != 1 || len(argLines(log)) != 1 || h.IndexAnyway(ServerCode) {
		t.Fatalf("asked %d, spawns %q", a.asked(), argLines(log))
	}
	if strings.Contains(savedLocal(ws), "index_anyway") {
		t.Errorf("saved: %s", savedLocal(ws))
	}
}

// A binary whose --help does not list --index-anyway is not offered it: one
// line, no question, and index-anyway on is refused.
func TestIndexAnyway_ProbeGates(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	a := &asker{ans: "index anyway"}
	h, log, msgs := startNotProject(t, ws, a.ask)
	msg := nextMsg(t, msgs)
	if a.asked() != 0 || !strings.Contains(msg, "is not a project — the code tools need one. start dun in a repository instead: cd a && dun") ||
		strings.Contains(msg, "index-anyway") {
		t.Fatalf("asked %d: %q", a.asked(), msg)
	}
	if _, _, err := h.SetIndexAnyway(context.Background(), ServerCode, true); err == nil || !strings.Contains(err.Error(), "takes no --index-anyway") {
		t.Fatalf("on for an old build: %v", err)
	}
	if h.IndexAnyway(ServerCode) || len(argLines(log)) != 1 {
		t.Fatalf("changed: %q", argLines(log))
	}

	// A saved yes does not reach a binary that would refuse the flag.
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) { s.IndexAnyway = true }); err != nil {
		t.Fatal(err)
	}
	h2, log2, msgs2 := startNotProject(t, ws, a.ask)
	nextMsg(t, msgs2)
	waitIndex(t, h2, IndexFailed)
	if a.asked() != 0 {
		t.Errorf("asked %d", a.asked())
	}
	for _, l := range argLines(log2) {
		if strings.Contains(l, "--index-anyway") {
			t.Errorf("an old build got --index-anyway: %s", l)
		}
	}
}

// With no one to ask (no Config.Ask), the same one line, and the command.
func TestIndexAnyway_NoAsker(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	_, _, msgs := startNotProject(t, t.TempDir(), nil, fakeAnywayEnv+"=1")
	msg := nextMsg(t, msgs)
	if !strings.Contains(msg, "cd a && dun") || !strings.Contains(msg, "/lsp index-anyway on indexes it regardless") {
		t.Fatalf("%q", msg)
	}
}

// leftOutBy reaches the server's state with the list beside it.
func TestIndex_LeftOutBy(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, _, msgs := startIndexed(t, t.TempDir(), fakeIndexEnv+"=leftoutby")
	nextMsg(t, msgs)
	st := waitIndex(t, h, IndexReady)
	if len(st.IndexLeftOutBy) != 3 || st.IndexLeftOutBy[0].Count != 8 || st.IndexLeftOutBy[2].Reason != "symlink loop" ||
		len(st.IndexLeftOut) != 3 {
		t.Fatalf("%+v", st)
	}
}
