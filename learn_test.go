package dun

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/proto"
)

const genericLesson = `{"name": "install missing build tools", "detail": "When a compiler or build tool is not found, refresh the system package index and install it before retrying the build.", "tags": ["build"]}`

// learnHarness is a session with a RagLink over f (nil: none, as under
// DUN_MEMORY=0) and a canned model.
func learnHarness(t *testing.T, f *fakeRag, reply string) (*Harness, *cannedRephraseRunner) {
	t.Helper()
	h, _ := itemHarness(t, f)
	r := &cannedRephraseRunner{reply: reply}
	h.Session.Runner = r
	return h, r
}

// step is one tool call through the plan nudge's counter: failed or not.
type step struct {
	tool, args string
	fail       bool
}

func runSteps(t *testing.T, h *Harness, steps ...step) {
	t.Helper()
	i := 0
	d := withPlanNudge(func(_ context.Context, tc llm.ToolCall) (string, error) {
		s := steps[i]
		i++
		if s.fail {
			return "ERROR: sh: 1: gcc: not found", nil
		}
		return "ok", nil
	}, h)
	for _, s := range steps {
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = s.tool, s.args
		if _, err := d(context.Background(), tc); err != nil {
			t.Fatal(err)
		}
	}
}

func execStep(cmd string, fail bool) step {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return step{tool: "exec", args: string(b), fail: fail}
}

func churn(n int) []step {
	var out []step
	for i := range n {
		out = append(out, execStep("make build-"+string(rune('a'+i)), true))
	}
	return append(out, execStep("apt-get update && apt-get install -y gcc && make", false))
}

// A streak of failed calls ended by a success on the same target is one
// episode: one call, a candidate strategy in the project's scope with the
// candidate tag, indexed, with a proposed entry in its log, not important,
// and left out of recall.
func TestLearn_EpisodeBecomesOneCandidate(t *testing.T) {
	f := &fakeRag{}
	h, r := learnHarness(t, f, "```json\n"+genericLesson+"\n```")
	runSteps(t, h, churn(3)...)
	outs := h.learnFlush(context.Background())
	if r.called != 1 || len(outs) != 1 || !strings.Contains(outs[0], "saved") {
		t.Fatalf("calls %d, outs %q", r.called, outs)
	}
	prompt := r.seen[0].Content
	if !strings.HasPrefix(prompt, learnInstruction) || !strings.Contains(prompt, "3 in a row") || !strings.Contains(prompt, "apt-get update") {
		t.Errorf("prompt:\n%s", prompt)
	}
	s := h.strategyStore()
	p := s.named(kindStrategy, "install-missing-build-tools")
	if p == "" || !isCandidate(p) {
		t.Fatalf("no candidate: %q", p)
	}
	n, _ := readNote(p)
	if n.meta["important"] == "true" || p[:len(s.dir)] != s.dir {
		t.Errorf("important or not in the project's scope: %v %s", n.meta, p)
	}
	in := ingestsOf(f)
	if len(in) != 1 || in[0].Index != strategiesIndex || !slices.Contains(in[0].Tags, candidateTag) || !slices.Contains(in[0].Tags, "kind:strategy") {
		t.Errorf("ingest: %+v", in)
	}
	if es := readAttest(p); len(es) != 1 || es[0].Kind != "proposed" || es[0].By != "dun:learn" {
		t.Errorf("attest: %+v", es)
	}
	if h.SideCalls()["learn"]["calls"] != 1 {
		t.Errorf("side calls: %v", h.SideCalls())
	}
	// Recall leaves it out; the dedup search sees it.
	f.mu.Lock()
	f.hits = map[string][]proto.RagHit{raglitProject(h.cfg.Workspace): {{Path: "file://" + p, Text: "x", Score: 1, Matched: 1, Terms: 1}}}
	f.mu.Unlock()
	if hits, _ := (&memFinder{l: h.stratLink(), m: s}).Find(context.Background(), []string{"build tool"}); len(hits) != 0 {
		t.Errorf("recall surfaced a candidate: %+v", hits)
	}
	if hits, _ := (&memFinder{l: h.stratLink(), m: s, candidates: true}).Find(context.Background(), []string{"build tool"}); len(hits) != 1 {
		t.Errorf("dedup search: %+v", hits)
	}
	// The same lesson again: one call, no second strategy.
	runSteps(t, h, churn(4)...)
	if outs := h.learnFlush(context.Background()); r.called != 2 || len(outs) != 1 || !strings.Contains(outs[0], "skipped") {
		t.Errorf("second: calls %d, %q", r.called, outs)
	}
}

// No episode: a streak that never ends, one too short, or one ended by a
// success on another target.
func TestLearn_UnresolvedStreaksMakeNothing(t *testing.T) {
	h, r := learnHarness(t, &fakeRag{}, genericLesson)
	read := step{tool: "file_read", args: `{"path":"a.txt"}`}
	runSteps(t, h, execStep("x", true), execStep("y", true), execStep("z", true), execStep("w", true))
	h.nudge.reset() // a new turn
	runSteps(t, h, execStep("x", true), execStep("y", true), execStep("z", false))
	runSteps(t, h, execStep("x", true), execStep("y", true), execStep("z", true), read)
	if outs := h.learnFlush(context.Background()); r.called != 0 || len(outs) != 0 {
		t.Errorf("calls %d, %q", r.called, outs)
	}
}

// At most learnCap lesson calls in a session, however many episodes.
func TestLearn_PerSessionCap(t *testing.T) {
	h, r := learnHarness(t, &fakeRag{}, genericLesson)
	for range learnCap + 2 {
		runSteps(t, h, churn(3)...)
	}
	h.learnFlush(context.Background())
	runSteps(t, h, churn(3)...)
	h.learnFlush(context.Background())
	if r.called != learnCap {
		t.Errorf("%d calls, cap %d", r.called, learnCap)
	}
}

// Off with no RagLink (DUN_MEMORY=0, as the Harbor adapter sets it).
func TestLearn_OffWithoutARagLink(t *testing.T) {
	h, r := learnHarness(t, nil, genericLesson)
	runSteps(t, h, churn(3)...)
	if outs := h.learnFlush(context.Background()); r.called != 0 || len(outs) != 0 {
		t.Errorf("calls %d, %q", r.called, outs)
	}
}

// A lesson about its task is dropped after the call: nothing is written.
func TestLearn_TaskBoundLessonIsRejected(t *testing.T) {
	h, r := learnHarness(t, &fakeRag{}, `{"name": "fix the build", "detail": "Run make in /app/povray-2.2 after editing src/config.h.", "tags": ["build"]}`)
	runSteps(t, h, churn(3)...)
	outs := h.learnFlush(context.Background())
	if r.called != 1 || len(outs) != 1 || !strings.Contains(outs[0], "rejected") {
		t.Fatalf("calls %d, %q", r.called, outs)
	}
	if l := h.strategyStore().list(); len(l) != 0 {
		t.Errorf("written: %+v", l)
	}
}

func TestLearn_GenericityFilter(t *testing.T) {
	names := map[string]bool{"render_scene": true, "render_scene.py": true, "src": true, "makefile": true}
	task := "Build POV-Ray 2.2 and render `illum1.pov`. The scene must match the reference image exactly."
	episode := "python3 -c 'import parseMoves; parseMoves.run()'\nTraceback: frame_buffer overflow"
	ok := []string{
		"When a compiler or build tool is not found, refresh the system package index and install it before retrying the build.",
		"Run the module with python3 -m pip when pip is not on the path. Check the makefile targets before guessing one.",
		"Set LD_LIBRARY_PATH when a built library is not found at run time.",
	}
	for _, d := range ok {
		if err := genericityError(lesson{Name: "x-y", Detail: d}, task, episode, names); err != nil {
			t.Errorf("rejected %q: %v", d, err)
		}
	}
	bad := map[string]string{
		"Edit /app/povray-2.2/source/config.h before running make.":                    "path",
		"Run make in src/unix/build first.":                                            "path",
		"Fix the import in render.py.":                                                 "file name",
		"Download it from https://example.com/x.tgz.":                                  "URL",
		"Check out commit 3f9a2c1d before building.":                                   "hash",
		"Call render_scene with the scene file.":                                       "workspace",
		"Build POV-Ray from its archive.":                                              "task",
		"The parseMoves module needs its run function.":                                "failed calls",
		"Grow frame_buffer before the second pass.":                                    "failed calls",
		"The scene must match the reference image exactly, so diff the output.":        "repeats the task",
		"Use \"for (int i = 0; i < n; i++) { buf[i] = 0; } return buf;\" to clear it.": "quotes",
		"One. Two. Three. Four.":                                                       "sentences",
		"Use ```make``` here.":                                                         "code",
	}
	for d, want := range bad {
		err := genericityError(lesson{Name: "x-y", Detail: d}, task, episode, names)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %s", d, err, want)
		}
	}
}

// The user reviews candidates: listed, not important until accepted;
// accept takes the tag off (a retag, no ingest) and recall finds it;
// reject voids it, and the same lesson is not proposed again.
func TestLearn_AcceptAndReject(t *testing.T) {
	f := &fakeRag{}
	h, r := learnHarness(t, f, genericLesson)
	runSteps(t, h, churn(3)...)
	h.learnFlush(context.Background())
	out, err := h.MemoriesCommand("candidates")
	if err != nil || !strings.Contains(out, "strategy:install-missing-build-tools") {
		t.Fatalf("candidates: %q %v", out, err)
	}
	if _, err := h.MemoriesCommand("important strategy:install-missing-build-tools"); err == nil || !strings.Contains(err.Error(), "accept it first") {
		t.Errorf("important on a candidate: %v", err)
	}
	if out, err := h.MemoriesCommand("accept strategy:install-missing-build-tools"); err != nil || !strings.Contains(out, "accepted") {
		t.Fatalf("accept: %q %v", out, err)
	}
	s := h.strategyStore()
	p := s.named(kindStrategy, "install-missing-build-tools")
	if isCandidate(p) {
		t.Error("still a candidate")
	}
	f.mu.Lock()
	tags := f.tags[len(f.tags)-1].Tags
	f.hits = map[string][]proto.RagHit{raglitProject(h.cfg.Workspace): {{Path: "file://" + p, Text: "x", Score: 1, Matched: 1, Terms: 1}}}
	f.mu.Unlock()
	if slices.Contains(tags, candidateTag) {
		t.Errorf("retag: %v", tags)
	}
	if hits, _ := (&memFinder{l: h.stratLink(), m: s}).Find(context.Background(), []string{"build tool"}); len(hits) != 1 {
		t.Errorf("accepted, not recalled: %+v", hits)
	}
	if _, err := h.MemoriesCommand("accept strategy:install-missing-build-tools"); err == nil {
		t.Error("accept of an accepted strategy")
	}
	if es := readAttest(p); es[len(es)-1].Kind != "confirmed" || es[len(es)-1].By != "user" {
		t.Errorf("attest: %+v", es)
	}

	// Reject: a second candidate.
	r.reply = `{"name": "check tool versions", "detail": "Print the version of each tool a build needs before starting a long build.", "tags": ["build"]}`
	f.mu.Lock()
	f.hits = nil
	f.mu.Unlock()
	runSteps(t, h, churn(3)...)
	h.learnFlush(context.Background())
	if out, err := h.MemoriesCommand("reject strategy:check-tool-versions"); err != nil || !strings.Contains(out, "voided") {
		t.Fatalf("reject: %q %v", out, err)
	}
	if s.named(kindStrategy, "check-tool-versions") != "" {
		t.Error("rejected, still current")
	}
	runSteps(t, h, churn(3)...)
	if outs := h.learnFlush(context.Background()); len(outs) != 1 || !strings.Contains(outs[0], "voided") {
		t.Errorf("re-proposed a rejected lesson: %q", outs)
	}
	if out, _ := h.MemoriesCommand("candidates"); !strings.Contains(out, "none") {
		t.Errorf("candidates after review: %q", out)
	}
}

// The model cannot set the candidate tag, nor drop it with a refine.
func TestLearn_ModelCannotTagOrUntagCandidates(t *testing.T) {
	h, _ := learnHarness(t, &fakeRag{}, genericLesson)
	if out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "strategy", "name": "x", "content": "Do x.", "tags": []string{"Candidate"}}); !strings.Contains(out, "learning phase only") {
		t.Errorf("add with the tag: %q", out)
	}
	runSteps(t, h, churn(3)...)
	h.learnFlush(context.Background())
	callTool(t, h, "memory", map[string]any{"action": "refine", "old_text": "strategy:install-missing-build-tools",
		"content": "Install missing build tools with the package manager.", "tags": []string{"build"}, "why": "shorter", "source": "code"})
	if p := h.strategyStore().named(kindStrategy, "install-missing-build-tools"); !isCandidate(p) {
		b, _ := os.ReadFile(p)
		t.Errorf("refine dropped the tag:\n%s", b)
	}
}
