package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// sessHarness is a session with a log file and a RagLink over rag, and the
// notices it gave.
func sessHarness(t *testing.T, rag RagLink) (*Harness, string, func() []string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	var mu sync.Mutex
	var notices []string
	cfg := Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s1.jsonl"),
		OnNotify: func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }}
	if rag != nil {
		cfg.Rag = rag
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, ws, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(notices) }
}

func putFold(t *testing.T, h *Harness, key, text string) {
	t.Helper()
	entries := []agent.Entry{{Kind: agent.KindUser, Content: "find the port"}, {Kind: agent.KindToolResult, Content: text}}
	if err := agent.PutSpan(context.Background(), lateSessionIndex{h}, agent.SessionDocFold, key, "", "Found it.", entries); err != nil {
		t.Fatal(err)
	}
}

// A fold's span is a file beside the session log, synced into dund's
// sessions index with the session's tag; session_search finds it there.
func TestSessionIndex_FoldThenSessionSearch(t *testing.T) {
	rag := &fakeRag{}
	h, ws, _ := sessHarness(t, rag)
	if !slices.Contains(modelToolNames(h), agent.SessionSearchName) {
		t.Fatalf("no session_search: %v", modelToolNames(h))
	}
	if !strings.Contains(h.Session.System, "session_search") {
		t.Error("the system prompt does not name session_search")
	}
	putFold(t, h, "fold-1", "port 7420 is the walrus service")
	file := filepath.Join(foldsDir(h.cfg.SessionFile), "fold-1.md")
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "7420") || !strings.Contains(string(b), "Found it.") {
		t.Fatalf("span file: %q %v", b, err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(rag.syncCalls()) == 1 })
	q := rag.syncCalls()[0]
	if q.Index != sessionsIndex || q.Project != mcpserver.RaglitProject(ws) || len(q.Files) != 1 || q.Files[0].Path != file ||
		!slices.Equal(q.Files[0].Tags, []string{"session:s1", "kind:fold"}) || q.Files[0].Title != "find the port" {
		t.Fatalf("sync %+v", q)
	}
	out := callTool(t, h, agent.SessionSearchName, map[string]any{"query": "walrus"})
	if !strings.Contains(out, "## fold-1 — find the port") || !strings.Contains(out, "7420") {
		t.Errorf("session_search: %q", out)
	}
	rag.mu.Lock()
	last := rag.searches[len(rag.searches)-1]
	rag.mu.Unlock()
	if !slices.Equal(last.Filter.Tags, []string{"session:s1"}) {
		t.Errorf("search filter %+v, want the session's tag", last.Filter)
	}
}

// No dund link (DUN_MEMORY=0): no session_search, and a fold puts nothing.
func TestSessionIndex_OffWithoutARagLink(t *testing.T) {
	h, _, _ := sessHarness(t, nil)
	if slices.Contains(modelToolNames(h), agent.SessionSearchName) {
		t.Error("session_search with no RagLink")
	}
	putFold(t, h, "fold-1", "x")
	if _, err := os.Stat(foldsDir(h.cfg.SessionFile)); !os.IsNotExist(err) {
		t.Errorf("span files with no RagLink: %v", err)
	}
}

// dund down at a fold: said once, the span kept; the next fold with dund
// back syncs both.
func TestSessionIndex_DundDownIsSaidAndCaughtUp(t *testing.T) {
	rag := &fakeRag{down: true}
	h, _, notices := sessHarness(t, rag)
	putFold(t, h, "fold-1", "first")
	putFold(t, h, "fold-2", "second")
	waitFor(t, 5*time.Second, func() bool { return len(notices()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	n := 0
	for _, s := range notices() {
		if strings.Contains(s, "session history: dund is not reachable") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("said %d times: %q", n, notices())
	}
	rag.setDown(false)
	putFold(t, h, "fold-3", "third")
	waitFor(t, 5*time.Second, func() bool { return len(rag.syncCalls()) == 1 })
	if files := rag.syncCalls()[0].Files; len(files) != 3 {
		t.Errorf("caught up %d files, want 3", len(files))
	}
}

// refusingRag is a dund from before the sessions index.
type refusingRag struct{ *fakeRag }

const oldDundRefusal = `rag: no index "sessions": a project has code, docs, memories, strategies`

func (r refusingRag) Sync(context.Context, proto.RagSync) (*proto.RagSynced, error) {
	return nil, errors.New(oldDundRefusal)
}

func (r refusingRag) Search(context.Context, proto.RagSearch) (*proto.RagHits, error) {
	return nil, errors.New(oldDundRefusal)
}

// An older dund: the notice and session_search each say it in one line.
func TestSessionIndex_RefusalIsSaid(t *testing.T) {
	h, _, notices := sessHarness(t, refusingRag{&fakeRag{}})
	putFold(t, h, "fold-1", "x")
	waitFor(t, 5*time.Second, func() bool {
		return slices.ContainsFunc(notices(), func(s string) bool { return strings.Contains(s, "not indexed: "+errOldDund.Error()) })
	})
	out := callTool(t, h, agent.SessionSearchName, map[string]any{"query": "x"})
	if out != "ERROR: session_search: "+errOldDund.Error() {
		t.Errorf("session_search: %q", out)
	}
}

// Spans already on disk (written while dund was down, or a resume) are
// synced when the session starts.
func TestSessionIndex_StartSyncsExistingFolds(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	sf := filepath.Join(t.TempDir(), "s1.jsonl")
	span := filepath.Join(foldsDir(sf), "fold-0.md")
	writeRule(t, span, "# old\n\nkind: fold\nkey: fold-0\n")
	rag := &fakeRag{}
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: sf, Rag: rag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	waitFor(t, 5*time.Second, func() bool { return len(rag.syncCalls()) == 1 })
	if f := rag.syncCalls()[0].Files; len(f) != 1 || f[0].Path != span || !slices.Contains(f[0].Tags, "kind:fold") {
		t.Errorf("start sync %+v", f)
	}
}

// vanishingRag fails its first sync as dund does when a file it was sent
// is gone, after removing it.
type vanishingRag struct {
	*fakeRag
	gone string
	once sync.Once
}

func (r *vanishingRag) Sync(ctx context.Context, q proto.RagSync) (*proto.RagSynced, error) {
	var err error
	r.once.Do(func() {
		os.Remove(r.gone)
		err = errors.New("rag: stat " + r.gone + ": no such file or directory")
	})
	if err != nil {
		return nil, err
	}
	return r.fakeRag.Sync(ctx, q)
}

// A span gone between the glob and dund's read does not cost the others,
// and leaves no half-written file behind.
func TestSessionIndex_VanishedSpanDoesNotAbortTheSync(t *testing.T) {
	rag := &vanishingRag{fakeRag: &fakeRag{}}
	t.Setenv("DUN_HOME", t.TempDir())
	sf := filepath.Join(t.TempDir(), "s1.jsonl")
	rag.gone = filepath.Join(foldsDir(sf), "fold-1.md")
	var mu sync.Mutex
	var notices []string
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: sf, Rag: rag,
		OnNotify: func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	putFold(t, h, "fold-1", "first")
	x := h.sessionIndex()
	x.wait(5 * time.Second)
	if err := x.write(agent.SessionDoc{Key: "fold-2", Kind: "fold", Body: "second"}); err != nil {
		t.Fatal(err)
	}
	x.sync(context.Background())
	calls := rag.syncCalls()
	if len(calls) == 0 {
		t.Fatal("no sync reached dund")
	}
	for _, f := range calls[len(calls)-1].Files {
		if f.Path == rag.gone {
			t.Errorf("the vanished span was sent again: %+v", calls)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 0 {
		t.Errorf("notices %q", notices)
	}
	if tmp, _ := filepath.Glob(filepath.Join(x.dir, ".*.tmp-*")); len(tmp) != 0 {
		t.Errorf("temp files left: %v", tmp)
	}
}

// Keys that sanitise alike stay apart, and a search returns the key as
// agentkit named it.
func TestSessionIndex_KeysStayDistinct(t *testing.T) {
	names := map[string]string{}
	for _, k := range []string{"a/b", "a-b", "a%2Fb", "a\\b", "", ".x", "%empty", "fold-1"} {
		n := foldFileName(k)
		if o, ok := names[n]; ok {
			t.Errorf("%q and %q both are %q", o, k, n)
		}
		if strings.ContainsAny(strings.TrimSuffix(n, ".md"), "/\\") || strings.HasPrefix(n, ".") {
			t.Errorf("%q is %q", k, n)
		}
		names[n] = k
	}
	rag := &fakeRag{}
	h, _, _ := sessHarness(t, rag)
	putFold(t, h, "fold/1", "the walrus port")
	waitFor(t, 5*time.Second, func() bool { return len(rag.syncCalls()) == 1 })
	out := callTool(t, h, agent.SessionSearchName, map[string]any{"query": "walrus"})
	if !strings.Contains(out, "## fold/1 — ") {
		t.Errorf("session_search: %q", out)
	}
}

// /close: the documents leave dund first, then ForgetSession takes the
// files; a prune (pruneTranscript) leaves the files.
func TestForgetSessionFolds(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := foldsDir(SessionFile(ws, "s1"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	span := filepath.Join(dir, "fold-1.md")
	writeRule(t, span, "# x\n")
	writeRule(t, SessionFile(ws, "s1"), "{}\n")
	rag := &fakeRag{}
	if err := ForgetSessionFolds(rag, ws, "s1"); err != nil {
		t.Fatal(err)
	}
	if q := rag.syncCalls(); len(q) != 1 || q[0].Index != sessionsIndex || q[0].Project != mcpserver.RaglitProject(ws) || !slices.Equal(q[0].Gone, []string{span}) {
		t.Fatalf("sync %+v", q)
	}
	if !pruneTranscript(ws, "s1") {
		t.Fatal("pruneTranscript")
	}
	if _, err := os.Stat(span); err != nil {
		t.Errorf("a prune removed the span file: %v", err)
	}
	ForgetSession(ws, "s1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ForgetSession left %s: %v", dir, err)
	}
	if err := ForgetSessionFolds(&fakeRag{down: true}, ws, "s1"); err != nil {
		t.Errorf("no spans, nothing to do: %v", err)
	}
}

// dund down at a forget: the documents are kept in the pending list and
// sent at the next forget that reaches dund; the list then goes.
func TestForgetSessionFolds_RetriedWhenDundIsBack(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	span := filepath.Join(foldsDir(SessionFile(ws, "s1")), "fold-1.md")
	writeRule(t, span, "# x\n")
	rag := &fakeRag{down: true}
	if err := ForgetSessionFolds(rag, ws, "s1"); err == nil || !strings.Contains(err.Error(), "kept to retry") {
		t.Fatalf("down: %v", err)
	}
	ForgetSession(ws, "s1")
	pending := pendingForgetFile(RootDir(ws))
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("no pending list: %v", err)
	}
	rag.setDown(false)
	if err := ForgetSessionFolds(rag, ws, "s2"); err != nil {
		t.Fatal(err)
	}
	if q := rag.syncCalls(); len(q) != 1 || !slices.Equal(q[0].Gone, []string{span}) || q[0].Project != mcpserver.RaglitProject(ws) {
		t.Fatalf("retry %+v", q)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Errorf("the pending list stayed: %v", err)
	}
}
