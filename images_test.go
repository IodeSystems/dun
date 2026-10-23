package dun

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// writePNG writes a w×h PNG; noisy fills it with random pixels so it does
// not compress (a photo saved as PNG).
func writePNG(t *testing.T, path string, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(x), uint8(y), 80, 255}
			if noisy {
				c = color.RGBA{uint8(r.IntN(256)), uint8(r.IntN(256)), uint8(r.IntN(256)), 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decodedSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return c.Width, c.Height
}

// A large image is scaled to imageMaxSide on its long side, keeping its
// aspect; a small one is sent as its own bytes, never upscaled.
func TestPrepareImage_Downscales(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.png")
	writePNG(t, big, 3136, 1000, false)
	mime, data, err := prepareImage(big)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, data); mime != "image/png" || w != imageMaxSide || h != 500 {
		t.Fatalf("got %s %d×%d, want image/png %d×500", mime, w, h, imageMaxSide)
	}

	small := filepath.Join(dir, "small.png")
	raw := writePNG(t, small, 40, 30, false)
	mime, data, err = prepareImage(small)
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || !bytes.Equal(data, raw) {
		t.Fatalf("a small PNG was re-encoded (%s, %d bytes vs %d)", mime, len(data), len(raw))
	}

	// A photo saved as PNG stays over the PNG cap after scaling: JPEG.
	photo := filepath.Join(dir, "photo.png")
	writePNG(t, photo, 2000, 1500, true)
	mime, data, err = prepareImage(photo)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodedSize(t, data); mime != "image/jpeg" || w != imageMaxSide || h != 1176 {
		t.Fatalf("got %s %d×%d, want image/jpeg %d×1176", mime, w, h, imageMaxSide)
	}

	if _, _, err := prepareImage(filepath.Join(dir, "missing.png")); err == nil {
		t.Fatal("a missing file read")
	}
}

// The text goes up to and including each label, then that image.
func TestImageParts_LabelsPlaceImages(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.png"), filepath.Join(dir, "b.png")
	writePNG(t, a, 4, 4, false)
	writePNG(t, b, 4, 4, false)
	task := "look [image 1: a.png 4×4] and [image 2: b.png 4×4] now"
	parts, err := imageParts(task, []ImageInput{{a, "[image 1: a.png 4×4]"}, {b, "[image 2: b.png 4×4]"}})
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, p := range parts {
		if p.Type == "image_url" {
			shape = append(shape, "IMG")
		} else {
			shape = append(shape, p.Text)
		}
	}
	want := []string{"look [image 1: a.png 4×4]", "IMG", " and [image 2: b.png 4×4]", "IMG", " now"}
	if strings.Join(shape, "|") != strings.Join(want, "|") {
		t.Fatalf("parts %q, want %q", shape, want)
	}
	var refused *ImageRefusedError
	if _, err := imageParts(task, []ImageInput{{filepath.Join(dir, "gone.png"), "[image 1: a.png 4×4]"}}); !errors.As(err, &refused) {
		t.Fatalf("an unreadable image: %v", err)
	}
}

// The session file holds a ref, not the image; a reload rebuilds the
// parts from the blob, and a blob that is gone becomes text.
func TestStore_ImagesLiveOutsideTheLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	st, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	img := llm.ImageData("image/png", []byte("\x89PNG fake bytes"))
	st.publish(agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "see [image 1: a.png 1×1]",
		Parts: []llm.ContentPart{llm.TextPart("see [image 1: a.png 1×1]"), img}})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("base64")) || bytes.Contains(raw, []byte("data:image")) {
		t.Fatalf("the log holds the image: %s", raw)
	}
	if st.entries[0].Parts[1].ImageURL.URL != img.ImageURL.URL {
		t.Fatal("the in-memory entry lost its image")
	}

	again, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := again.entries[0].Parts
	if len(ps) != 2 || ps[1].ImageURL == nil || ps[1].ImageURL.URL != img.ImageURL.URL {
		t.Fatalf("resume did not rebuild the image: %+v", ps)
	}

	blobs, _ := filepath.Glob(filepath.Join(dir, "blobs", "*.png"))
	if len(blobs) != 1 {
		t.Fatalf("blobs: %v", blobs)
	}
	os.Remove(blobs[0])
	gone, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ps = gone.entries[0].Parts
	if len(ps) != 2 || ps[1].Type != "text" || !strings.Contains(ps[1].Text, "image missing") {
		t.Fatalf("a missing image: %+v", ps)
	}
}

func TestRefusesImages(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"llm: status 400: Invalid content type. image_url is only supported by certain models.", true},
		{`llm: status 400: {"error":"Qwen3 is not a multimodal model"}`, true},
		{"llm: status 404: No endpoints found that support image input", true},
		{"llm: status 500 (after 4 retries): image input is not supported - hint: provide the mmproj", true},
		{"llm: status 400: tool schema rejected", false},
		{"llm: status 429: images per minute exceeded", false},
		{"llm: status 503: vision backend overloaded", false},
		{"dial tcp: connection refused", false},
	} {
		if got := refusesImages(errors.New(c.err)); got != c.want {
			t.Errorf("%q: %v, want %v", c.err, got, c.want)
		}
	}
}

// visionRunner is a model that refuses images (vision false) or reads them,
// and records what it was sent.
type visionRunner struct {
	mu     sync.Mutex
	vision bool
	sent   []llm.Message // the last user message of each call
	images int           // calls that carried an image
}

func (r *visionRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var last llm.Message
	for _, m := range msgs {
		if m.Role == "user" {
			last = m
		}
	}
	r.sent = append(r.sent, last)
	if len(last.Parts) > 0 {
		r.images++
		if !r.vision {
			return nil, errors.New("llm: status 400: Invalid content type. image_url is only supported by certain models.")
		}
	}
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "ok"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func imageHarness(t *testing.T, runner agent.LLMRunner, v VisionConfig) (*Harness, string) {
	t.Helper()
	dir := t.TempDir()
	if v.StateFile == "" {
		v.StateFile = filepath.Join(dir, "vision.json")
	}
	v.Key = "http://llm test-model"
	h, err := Start(context.Background(), Config{Workspace: dir, Servers: []Server{}, Client: runner,
		SessionFile: filepath.Join(dir, "s.jsonl"), Vision: v})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	img := filepath.Join(dir, "shot.png")
	writePNG(t, img, 8, 8, false)
	return h, img
}

// The first image to a model that reads them is sent as an image, and the
// record says vision; the next is sent the same way on the record's word.
func TestAskImages_ProbeRecordsVision(t *testing.T) {
	r := &visionRunner{vision: true}
	h, img := imageHarness(t, r, VisionConfig{})
	in := []ImageInput{{img, "[image 1: shot.png 8×8]"}}
	if _, err := h.AskImages(context.Background(), "what is [image 1: shot.png 8×8]", in); err != nil {
		t.Fatal(err)
	}
	if known, vision := h.cfg.Vision.lookup(); !known || !vision {
		t.Fatalf("record: known=%v vision=%v", known, vision)
	}
	if r.images != 1 || r.sent[0].Parts[1].ImageURL == nil {
		t.Fatalf("the image was not sent: %d", r.images)
	}
	if _, err := h.AskImages(context.Background(), "and [image 1: shot.png 8×8]", in); err != nil {
		t.Fatal(err)
	}
	if r.images < 2 {
		t.Fatal("the second image was not sent")
	}
}

// describer stands in for the vision model.
type describer struct{ calls int }

func (d *describer) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	d.calls++
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "a red square on white"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// A model that refuses the probe is recorded without vision, the image goes
// as the vision model's description, and the next image is described
// without being offered to the model again.
func TestAskImages_NoVisionDescribes(t *testing.T) {
	r := &visionRunner{}
	d := &describer{}
	h, img := imageHarness(t, r, VisionConfig{Describer: d, DescriberModel: "vl"})
	in := []ImageInput{{img, "[image 1: shot.png 8×8]"}}
	if _, err := h.AskImages(context.Background(), "fix [image 1: shot.png 8×8]", in); err != nil {
		t.Fatal(err)
	}
	if known, vision := h.cfg.Vision.lookup(); !known || vision {
		t.Fatalf("record: known=%v vision=%v", known, vision)
	}
	last := r.sent[len(r.sent)-1]
	want := "fix [image 1: shot.png 8×8 — description: a red square on white]"
	if len(last.Parts) != 0 || last.Content != want {
		t.Fatalf("sent %+v, want text %q", last, want)
	}
	if r.images != 1 || d.calls != 1 {
		t.Fatalf("images to the model %d, descriptions %d", r.images, d.calls)
	}
	// What the session keeps is the description: a resume does not describe again.
	raw, _ := os.ReadFile(h.cfg.SessionFile)
	if !bytes.Contains(raw, []byte("description: a red square")) {
		t.Fatalf("the log has no description: %s", raw)
	}

	if _, err := h.AskImages(context.Background(), "again [image 1: shot.png 8×8]", in); err != nil {
		t.Fatal(err)
	}
	if r.images != 1 || d.calls != 2 {
		t.Fatalf("re-probed: images to the model %d, descriptions %d", r.images, d.calls)
	}
}

// No vision and no vision model: the message is refused, says how to set
// one, and is not left in the session.
func TestAskImages_NoVisionNoDescriberRefuses(t *testing.T) {
	r := &visionRunner{}
	h, img := imageHarness(t, r, VisionConfig{})
	in := []ImageInput{{img, "[image 1: shot.png 8×8]"}}
	_, err := h.AskImages(context.Background(), "fix [image 1: shot.png 8×8]", in)
	var refused *ImageRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, `"vision_model"`) {
		t.Fatalf("err %v", err)
	}
	for _, e := range h.store.entries {
		if e.Kind == agent.KindUser {
			t.Fatalf("the refused message stayed in the session: %q", e.Content)
		}
	}
	if h.ImagesRefused() == "" {
		t.Fatal("the next message with images is not refused up front")
	}
}
