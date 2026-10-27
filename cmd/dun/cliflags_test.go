package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

// --no-rag / --no-lsp are --rag=false / --lsp=false: the same override, so the
// two spellings cannot drift. A contradiction is an error.
func TestNoRagNoLsp_AreTheOffSpelling(t *testing.T) {
	var rag, lsp tristate
	if err := noAutostart(&rag, true, "rag"); err != nil {
		t.Fatal(err)
	}
	if err := noAutostart(&lsp, false, "lsp"); err != nil {
		t.Fatal(err)
	}
	got := autostartOverrides(rag, lsp)
	if v, ok := got[dun.ServerDocs]; !ok || v || len(got) != 1 {
		t.Errorf("--no-rag: %v", got)
	}
	var off tristate
	_ = off.Set("false")
	if rag.String() != off.String() {
		t.Errorf("--no-rag passes %q to the engine, --rag=false passes %q", rag.String(), off.String())
	}
	var on tristate
	_ = on.Set("true")
	if err := noAutostart(&on, true, "rag"); err == nil {
		t.Error("--rag --no-rag was accepted")
	}
	// --rag=false --no-rag says the same twice: fine.
	if err := noAutostart(&off, true, "rag"); err != nil {
		t.Error(err)
	}
}

// The flags reach the engine's argv, and dund's argv filter, for the TUI.
func TestFlags_PassToTheEngine(t *testing.T) {
	o := tuiOpts{workspace: "/ws", suggest: true, rag: "false", lsp: "false", noTools: true}
	for _, mode := range []string{"-p", "--tui"} {
		args := procArgs(o, mode)
		for _, want := range []string{"--no-tools", "--rag=false", "--lsp=false"} {
			if !slices.Contains(args, want) {
				t.Errorf("%s argv lacks %s: %v", mode, want, args)
			}
		}
	}
	eng := engineArgs(o)
	if !slices.Contains(eng, "--no-tools") || !slices.Contains(eng, "--rag=false") {
		t.Errorf("dund engine args: %v", eng)
	}
	if slices.Contains(procArgs(tuiOpts{workspace: "/ws", suggest: true}, "-p"), "--no-tools") {
		t.Error("--no-tools without being asked")
	}
}

// The flags exist and are listed by --help; --media is repeatable, needs a
// one-shot task, and refuses a file that is not an image.
func TestFlags_RealBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dun")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "DUN_NO_SELFUPDATE=1")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	help := run("--help")
	for _, n := range []string{"--no-tools", "--no-rag", "--no-lsp", "--media"} {
		if !strings.Contains(help, n) {
			t.Errorf("--help lacks %s", n)
		}
	}
	dir := t.TempDir()
	txt := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(txt, []byte("hello"), 0o644)
	if out := run("--media", txt, "x"); !strings.Contains(out, "png, jpeg, gif, webp") {
		t.Errorf("a text file: %s", out)
	}
	png := filepath.Join(dir, "a.png")
	writeTestPNG(t, png)
	if out := run("--media", png); !strings.Contains(out, "one-shot task") {
		t.Errorf("no task: %s", out)
	}
	if out := run("--rag", "--no-rag", "x"); !strings.Contains(out, "contradict") {
		t.Errorf("--rag --no-rag: %s", out)
	}
}

func TestMediaFlag_Repeats(t *testing.T) {
	var m mediaFlag
	_ = m.Set("a.png")
	_ = m.Set("b.png")
	if len(m) != 2 {
		t.Errorf("--media is not repeatable: %v", m)
	}
	if m.Set("") == nil {
		t.Error("--media '' was accepted")
	}
}

func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMedia_ResolveRefusesWhatIsNotAnImage(t *testing.T) {
	dir := t.TempDir()
	good, text := filepath.Join(dir, "shot.png"), filepath.Join(dir, "notes.txt")
	writeTestPNG(t, good)
	_ = os.WriteFile(text, []byte("hello"), 0o644)
	m := mediaFlag{good}
	if err := m.resolve(); err != nil {
		t.Fatal(err)
	}
	if in := m.inputs(); len(in) != 1 || in[0].Path != good {
		t.Errorf("inputs: %v", in)
	}
	err := (mediaFlag{text}).resolve()
	if err == nil || !strings.Contains(err.Error(), "png, jpeg, gif, webp") || !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("a text file: %v", err)
	}
	if err := (mediaFlag{filepath.Join(dir, "gone.png")}).resolve(); err == nil {
		t.Error("a missing file was accepted")
	}
}

// capture records the last user message of each request.
type capture struct {
	mu   sync.Mutex
	sent []llm.Message
}

func (c *capture) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	c.mu.Lock()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			c.sent = append(c.sent, msgs[i])
			break
		}
	}
	c.mu.Unlock()
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "ok"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// The one-shot's first message carries the media; the harness is a plain
// chat when --no-tools.
func TestRunHuman_AttachesMediaToTheFirstMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	writeTestPNG(t, img)
	c := &capture{}
	h, err := dun.Start(context.Background(), dun.Config{Workspace: dir, Client: c, NoTools: true,
		SessionFile: filepath.Join(dir, "s.jsonl"),
		Vision:      dun.VisionConfig{Key: "http://llm m", StateFile: filepath.Join(dir, "vision.json")}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if n := len(h.ToolNames()); n != 0 {
		t.Errorf("tools under --no-tools: %v", h.ToolNames())
	}
	reply := runHuman(context.Background(), h, "what is wrong here", dir, mediaFlag{img}.inputs(), true)
	if reply != "ok" {
		t.Errorf("reply %q", reply)
	}
	if len(c.sent) != 1 {
		t.Fatalf("requests: %d", len(c.sent))
	}
	var text, images int
	for _, p := range c.sent[0].Parts {
		if p.ImageURL != nil {
			images++
		}
		text += len(p.Text)
	}
	if images != 1 || text == 0 {
		t.Errorf("parts: %d images, %d text bytes", images, text)
	}
}
