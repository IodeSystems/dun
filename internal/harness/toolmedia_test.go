package harness

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/memory"
	"github.com/iodesystems/dun/internal/mounttool"
)

// shotPNG is a tool result's image: the bytes do not matter to a wrapper.
var shotPNG = llm.ImageData("image/png", []byte("\x89PNG\r\n\x1a\nnot really"))

func mediaCall(name, args string) llm.ToolCall {
	tc := llm.ToolCall{ID: "m1"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

// Every dispatcher wrapper hands a result's media parts on unchanged: a
// wrapper that adds a line (a cd nudge, a lifted note) changes Text only.
func TestWrappersKeepMediaParts(t *testing.T) {
	h, ts, _ := editHarness(t)
	inner := func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
		return llm.ToolResult{Text: "took a screenshot", Parts: []llm.ContentPart{shotPNG}}, nil
	}
	report := func(string, map[string]any, string) {}
	wraps := []struct {
		name string
		wrap func(agent.ToolDispatcher) agent.ToolDispatcher
	}{
		{"attached", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withAttached(d, nil) }},
		{"codeguard", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withCodeGuard(d, h, nil) }},
		{"cdnudge", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withCdNudge(d, h.cfg.Workspace) }},
		{"files", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return withFiles(d, ts, nil, h, h.readWhole, report)
		}},
		{"ask", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return withAskGuard(withAsk(d, func(context.Context, string, []string, bool, bool, []string) (string, error) {
				return "", nil
			}, report), h)
		}},
		{"mount", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return mounttool.Wrap(d, func(context.Context, string, string) (bool, error) { return false, nil }, report)
		}},
		{"agent", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withAgent(d, h, report) }},
		{"tell_parent", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return withAskParent(withTellParent(d, h, report), h, report)
		}},
		{"jobs", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withJobs(d, h, report) }},
		{"recap", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withRecap(d, h, report) }},
		{"memory", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return memory.WithItemRead(memory.WithTool(d, func(string) string { return "" }, report),
				func() memory.Stores { return memory.Stores{} })
		}},
		{"session_search", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return withSessionSearch(d, func(context.Context, string) string { return "" }, report)
		}},
		{"skill", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withSkill(d, h, report) }},
		{"asks", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withAsks(d, h, report) }},
		{"recapwatch", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withRecapWatch(d, h) }},
		{"plannudge", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withPlanNudge(d, h) }},
		{"idlerearm", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withIdleRearm(d, h) }},
		{"liftedqueue", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withLiftedQueue(d, h) }},
		{"requiredargs", func(d agent.ToolDispatcher) agent.ToolDispatcher {
			return withRequiredArgs(d, func() []llm.ToolDef { return nil })
		}},
		{"plangate", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withPlanGate(d, h, map[string]string{}) }},
		{"loopguard", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withLoopGuard(d, h) }},
		{"interrupt", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withInterrupt(d) }},
		{"edits", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withEdits(d, h, ts) }},
		{"toolevents", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withToolEvents(d, h) }},
		{"ledger", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withLedger(d, h) }},
		{"callinfo", func(d agent.ToolDispatcher) agent.ToolDispatcher { return withCallInfo(d, h) }},
	}
	// An MCP tool every wrapper routes onward; exec for the cd nudge, which
	// adds its line to an exec result.
	calls := []llm.ToolCall{mediaCall("shots__take", `{}`),
		mediaCall("exec", `{"command":"cd `+h.cfg.Workspace+` && shoot"}`)}
	chain := agent.ToolDispatcher(inner)
	for _, w := range wraps {
		chain = w.wrap(chain)
		for _, tc := range calls {
			got, err := w.wrap(inner)(context.Background(), tc)
			if err != nil {
				t.Fatalf("%s %s: %v", w.name, tc.Function.Name, err)
			}
			if !reflect.DeepEqual(got.Parts, []llm.ContentPart{shotPNG}) {
				t.Errorf("%s %s: parts %v", w.name, tc.Function.Name, got.Parts)
			}
			if !strings.HasPrefix(got.Text, "took a screenshot") {
				t.Errorf("%s %s: text %q", w.name, tc.Function.Name, got.Text)
			}
		}
	}
	// All of them at once, in the order the session builds them.
	h.Notify("background job #1 finished")
	got, err := chain(context.Background(), calls[1])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Parts, []llm.ContentPart{shotPNG}) {
		t.Errorf("chain: parts %v", got.Parts)
	}
	for _, want := range []string{"took a screenshot", "background job #1 finished", "Each exec starts in the project dir"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("chain: text is missing %q:\n%s", want, got.Text)
		}
	}
}

// The tool's end event and the history show a label where an image was,
// never its base64.
func TestToolResultImageShownAsLabel(t *testing.T) {
	r := llm.ToolResult{Text: "took a screenshot", Parts: []llm.ContentPart{shotPNG}}
	got := resultShown(r)
	if got != "took a screenshot\n[image/png, 18 B]" {
		t.Errorf("shown %q", got)
	}
	if strings.Contains(got, "base64") {
		t.Errorf("base64 in %q", got)
	}
	if got := resultShown(llm.ToolResult{Parts: []llm.ContentPart{shotPNG}}); got != "[image/png, 18 B]" {
		t.Errorf("image only: %q", got)
	}
	if got := resultShown(llm.TextResult("plain")); got != "plain" {
		t.Errorf("text only: %q", got)
	}

	h, _, ends := editHarness(t)
	inner := func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return r, nil }
	if _, err := withToolEvents(inner, h)(context.Background(), mediaCall("shots__take", `{}`)); err != nil {
		t.Fatal(err)
	}
	if len(*ends) != 1 || (*ends)[0].Result != "took a screenshot\n[image/png, 18 B]" {
		t.Errorf("end events %+v", *ends)
	}

	h.store.publish(agent.Entry{ID: "c", Kind: agent.KindToolCall, ToolCallID: "m1", ToolName: "shots__take", Content: `{}`})
	h.store.publish(agent.Entry{ID: "r", Kind: agent.KindToolResult, ToolCallID: "m1", ToolName: "shots__take",
		Content: r.Text, Parts: r.Parts})
	var found bool
	for _, it := range h.History() {
		if it.Kind == "tool_result" {
			found = true
			if it.Content != "took a screenshot\n[image/png, 18 B]" {
				t.Errorf("history %q", it.Content)
			}
		}
	}
	if !found {
		t.Error("no tool_result in the history")
	}
}

// visionHarness is a harness whose model is recorded with vision or without.
func visionHarness(t *testing.T, vision bool, describer agent.LLMRunner) *Harness {
	t.Helper()
	h := newNoteHarness(t)
	h.cfg.Vision = VisionConfig{Key: "http://m model", Model: "model",
		StateFile: filepath.Join(t.TempDir(), "vision.json"), Describer: describer, DescriberModel: "vl"}
	h.cfg.Vision.record(vision, "")
	return h
}

// A model recorded without vision gets each image in a tool result as the
// describer's text; a model with vision, or not yet probed, gets the image.
func TestPrepareToolResultDescribesForNoVision(t *testing.T) {
	r := llm.ToolResult{Text: "took a screenshot", Parts: []llm.ContentPart{shotPNG, llm.AudioData("wav", []byte("RIFF"))}}
	tc := mediaCall("shots__take", `{}`)

	var asked []llm.Message
	desc := &fakeDescriber{reply: "A terminal showing   FAIL: TestX", seen: &asked}
	h := visionHarness(t, false, desc)
	got, err := h.prepareToolResult(context.Background(), tc, r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "took a screenshot\n[image/png from shots__take — description: A terminal showing FAIL: TestX]"; got.Text != want {
		t.Errorf("text %q\nwant %q", got.Text, want)
	}
	if len(got.Parts) != 1 || got.Parts[0].Type != "input_audio" {
		t.Errorf("parts %v: the audio part stays, the image goes", got.Parts)
	}
	if len(asked) != 1 || len(asked[0].Parts) != 2 || !strings.Contains(asked[0].Parts[0].Text, "shots__take\ntook a screenshot") ||
		asked[0].Parts[1].Type != "image_url" {
		t.Errorf("describer asked %+v", asked)
	}

	// No describer: a label says the image was left out, and why.
	h = visionHarness(t, false, nil)
	got, _ = h.prepareToolResult(context.Background(), tc, llm.ToolResult{Parts: []llm.ContentPart{shotPNG}})
	if want := "[image/png left out: model cannot read images, and no vision model is set to describe them]"; got.Text != want || got.Parts != nil {
		t.Errorf("no describer: %q %v", got.Text, got.Parts)
	}

	// The describer fails: left out, with its error.
	h = visionHarness(t, false, &fakeDescriber{err: errors.New("503 busy")})
	got, _ = h.prepareToolResult(context.Background(), tc, r)
	if !strings.Contains(got.Text, "left out") || !strings.Contains(got.Text, "503 busy") {
		t.Errorf("describer failed: %q", got.Text)
	}

	// Vision, and not probed: unchanged.
	h = visionHarness(t, true, desc)
	if got, _ := h.prepareToolResult(context.Background(), tc, r); !reflect.DeepEqual(got, r) {
		t.Errorf("vision: %+v", got)
	}
	h = newNoteHarness(t)
	h.cfg.Vision = VisionConfig{Key: "k", StateFile: filepath.Join(t.TempDir(), "vision.json")}
	if got, _ := h.prepareToolResult(context.Background(), tc, r); !reflect.DeepEqual(got, r) {
		t.Errorf("not probed: %+v", got)
	}
}

// fakeDescriber is a vision model: it replies, or fails, and keeps what it
// was asked.
type fakeDescriber struct {
	reply string
	err   error
	seen  *[]llm.Message
}

func (f *fakeDescriber) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, msgs...)
	}
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: f.reply}
	close(ch)
	return ch, nil
}
