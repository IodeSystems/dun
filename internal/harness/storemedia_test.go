package harness

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"
)

// A tool result's media parts (image, audio, video, PDF) go to blobs/, not
// into the session file, and come back whole when the session is opened.
func TestStoreToolResultMediaInBlobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	st, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := []llm.ContentPart{
		llm.ImageData("image/png", []byte("\x89PNG\r\n\x1a\nimage bytes")),
		llm.AudioData("wav", []byte("RIFF....WAVEaudio bytes")),
		llm.VideoData("video/mp4", []byte("....ftypmp42video bytes")),
		llm.PDFData("spec.pdf", []byte("%PDF-1.7 pdf bytes")),
	}
	st.publish(agent.Entry{ID: "c", Kind: agent.KindToolCall, ToolCallID: "t1", ToolName: "file_read", Content: `{"path":"x"}`})
	st.publish(agent.Entry{ID: "r", Kind: agent.KindToolResult, ToolCallID: "t1", ToolName: "file_read",
		Content: "x: four media files", Parts: parts})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"image bytes", "audio bytes", "video bytes", "pdf bytes"} {
		if strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte(p))[:12]) {
			t.Errorf("the session file holds %q as base64", p)
		}
	}
	if strings.Contains(string(raw), "base64,") {
		t.Errorf("a data: URI in the session file:\n%s", raw)
	}
	blobs, _ := os.ReadDir(filepath.Join(filepath.Dir(path), "blobs"))
	if len(blobs) != 4 {
		t.Errorf("blobs: %d, want 4", len(blobs))
	}

	again, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := again.Context(context.Background(), "")
	var got []llm.ContentPart
	for _, e := range ents {
		if e.Kind == agent.KindToolResult {
			got = e.Parts
		}
	}
	if !reflect.DeepEqual(got, parts) {
		t.Errorf("reloaded parts differ:\n got %+v\nwant %+v", got, parts)
	}

	// A blob gone: a text part says so.
	for _, b := range blobs {
		if strings.HasSuffix(b.Name(), ".mp4") {
			_ = os.Remove(filepath.Join(filepath.Dir(path), "blobs", b.Name()))
		}
	}
	again, _ = openSessionStore(path)
	ents, _ = again.Context(context.Background(), "")
	for _, e := range ents {
		if e.Kind == agent.KindToolResult && (e.Parts[2].Type != "text" || !strings.Contains(e.Parts[2].Text, "video missing")) {
			t.Errorf("missing video: %+v", e.Parts[2])
		}
	}
}

// An image read rounds ago renders as a label naming media:<hex>; file_read
// on that path gives the stored image back while the entry is in context.
func TestStubbedImageIsRecallable(t *testing.T) {
	h, _, _ := editHarness(t)
	h.cfg.Vision = VisionConfig{Key: "k", StateFile: filepath.Join(t.TempDir(), "vision.json")}
	h.Session = &agent.Session{Store: h.store}
	img := llm.ImageData("image/png", []byte("\x89PNG\r\n\x1a\nscreenshot"))
	var at int64
	add := func(e agent.Entry) { at++; e.CreatedAt = at; h.store.publish(e) }
	add(agent.Entry{ID: "u", Kind: agent.KindUser, Content: "look at the screenshot"})
	add(agent.Entry{ID: "c0", Kind: agent.KindToolCall, ToolCallID: "t0", ToolName: "file_read", Content: `{"path":"shot.png"}`})
	add(agent.Entry{ID: "r0", Kind: agent.KindToolResult, ToolCallID: "t0", ToolName: "file_read",
		Content: "shot.png: image/png", Parts: []llm.ContentPart{img}})
	for i := 1; i <= 40; i++ {
		add(agent.Entry{ID: fmt.Sprintf("a%d", i), Kind: agent.KindAssistant, Content: fmt.Sprintf("step %d", i)})
		add(agent.Entry{ID: fmt.Sprintf("c%d", i), Kind: agent.KindToolCall, ToolCallID: fmt.Sprintf("t%d", i),
			ToolName: "exec", Content: `{"command":"true"}`})
		add(agent.Entry{ID: fmt.Sprintf("r%d", i), Kind: agent.KindToolResult, ToolCallID: fmt.Sprintf("t%d", i),
			ToolName: "exec", Content: "ok"})
	}
	sh := &agent.Shaper{Store: h.store}
	setMediaStub(&sh.Policy)
	msgs, err := sh.Build(context.Background(), "", "system")
	if err != nil {
		t.Fatal(err)
	}
	ref := llm.MediaRef(img)
	var label string
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "image_url" {
				t.Fatalf("the image is still resent: %s", m.Role)
			}
			if strings.Contains(p.Text, ref) {
				label = p.Text
			}
		}
		if strings.Contains(m.Content, ref) {
			label = m.Content
		}
	}
	if !strings.Contains(label, `call file_read with path "`+ref+`"`) {
		t.Fatalf("no stub label naming %s and file_read: %q", ref, label)
	}

	// The model follows the label.
	cfg, _ := h.filesConfig()
	cfg.Tools = []string{filetools.Read}
	ts, err := filetools.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := withFiles(func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.TextResult("not mine"), nil },
		ts, nil, h, h.readWhole, nil)
	res, err := d(context.Background(), mediaCall(ts.Name(filetools.Read), `{"path":"`+ref+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parts) != 1 || !reflect.DeepEqual(res.Parts[0], img) {
		t.Fatalf("recall: %q parts %v", res.Text, res.Parts)
	}
}
