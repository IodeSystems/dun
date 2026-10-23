package dun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders: registered for image.Decode
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// images.go — images on a user message.
//
// A client attaches an image as a FILE on this machine plus the label that
// stands for it in the text ("[image 1: shot.png 1920×1080]"). The engine
// reads the file, scales it, and sends the message as content parts: the
// text up to and including each label, then that image. The label stays in
// the text, so the model can say "image 1", and the transcript, the
// history and every text-only consumer read the label where the image was.
//
// The session log never holds the image: the store writes it to the
// session's blobs/ by content hash and keeps a ref (store.go).
//
// Whether the model reads images at all is not known in advance, and the
// provider says so only by refusing one. So the first image sent to a
// model is the probe: a refusal records vision=false for it, a turn that
// went through records vision=true, both in $DUN_HOME/vision.json, and
// later sends use what is recorded. A model without vision gets each image
// as a DESCRIPTION written by the configured vision model (Config.Vision);
// with none configured, the message is not sent at all — an image the
// model never saw, sent as if it had, is worse than a refusal.

// ImageInput is one image attached to a user message.
type ImageInput struct {
	Path  string // a file on this machine
	Label string // the text that stands for it in the message
}

// VisionConfig is what image input needs from the caller.
type VisionConfig struct {
	// Key names the model Client runs, for the probe record: provider URL
	// and model id (VisionKey). Empty: probe results are not kept.
	Key string
	// Model is the model's id, for messages.
	Model string
	// Describer is the vision→text model that describes images for a model
	// that cannot read them; DescriberModel is its id. nil: none configured.
	Describer      agent.LLMRunner
	DescriberModel string
	// StateFile is where probe results are kept; "" is $DUN_HOME/vision.json.
	StateFile string
}

// VisionKey is the probe record's key for a model on an endpoint.
func VisionKey(url, model string) string {
	return strings.TrimRight(strings.TrimSpace(url), "/") + " " + strings.TrimSpace(model)
}

// imageMaxSide is the longest side an image is sent at. Anthropic's
// guidance for its vision models (larger is scaled down server side, at the
// same token cost), and close to where other providers tile; a 4K
// screenshot sent whole costs several times the tokens for no more detail
// the model uses.
const imageMaxSide = 1568

// imagePNGMax: a PNG still larger than this after scaling (a photo saved as
// PNG) is sent as JPEG at imageJPEGQuality instead. Screenshots stay PNG —
// text in them survives PNG and blurs in JPEG.
const (
	imagePNGMax      = 1 << 20
	imageJPEGQuality = 85
)

// ImageRefusedError is a user message with images that was not sent:
// the model cannot read images and nothing can describe them, or an image
// could not be read. The message is the user's to fix and send again.
type ImageRefusedError struct{ Msg string }

func (e *ImageRefusedError) Error() string { return e.Msg }

// noVisionMsg says the model has no vision and how to configure the
// vision model that would describe images for it.
func noVisionMsg(model string) string {
	if model == "" {
		model = "this model"
	}
	return model + " cannot read images, and no vision model is set to describe them — the message was not sent. " +
		"Set one in " + filepath.Join(DunHome(), "config.json") + `: "vision_model": "<a model id that reads images>", ` +
		`for example "vision_model": "qwen2.5-vl-7b-instruct" (on the active provider; add "vision_provider": "<name>" ` +
		"for a model on another logged-in provider), then restart the session"
}

// prepareImage reads an image file and returns it as it is sent: at most
// imageMaxSide on its long side, PNG or JPEG.
//
// Never upscaled. A PNG or JPEG that needs no scaling is sent as its own
// bytes; anything else (GIF, WebP, a scaled image) is re-encoded — as PNG,
// unless that comes out over imagePNGMax, then as JPEG.
func prepareImage(path string) (mime string, data []byte, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return scaleImage(raw)
}

func scaleImage(raw []byte) (mime string, data []byte, err error) {
	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("not an image dun can read (png, jpeg, gif, webp): %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scaled := false
	if long := max(w, h); long > imageMaxSide {
		nw, nh := w*imageMaxSide/long, h*imageMaxSide/long
		dst := image.NewRGBA(image.Rect(0, 0, max(1, nw), max(1, nh)))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
		src, scaled = dst, true
	}
	if !scaled && (format == "png" && len(raw) <= imagePNGMax || format == "jpeg") {
		return "image/" + format, raw, nil
	}
	if format != "jpeg" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			return "", nil, err
		}
		if buf.Len() <= imagePNGMax {
			return "image/png", buf.Bytes(), nil
		}
	}
	// JPEG has no alpha: flatten onto white, which is what a viewer shows a
	// transparent screenshot on.
	flat := image.NewRGBA(src.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, src.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
		return "", nil, err
	}
	return "image/jpeg", buf.Bytes(), nil
}

// imageParts builds the message's content parts: the text up to and
// including each image's label, then the image; the rest of the text last.
// An image whose label is not in the text goes after it.
func imageParts(task string, imgs []ImageInput) ([]llm.ContentPart, error) {
	var parts []llm.ContentPart
	var trailing []llm.ContentPart
	rest := task
	for _, im := range imgs {
		mime, data, err := prepareImage(im.Path)
		if err != nil {
			return nil, &ImageRefusedError{Msg: fmt.Sprintf("%s: could not read %s: %v — the message was not sent", im.Label, im.Path, err)}
		}
		part := llm.ImageData(mime, data)
		i := strings.Index(rest, im.Label)
		if im.Label == "" || i < 0 {
			trailing = append(trailing, part)
			continue
		}
		cut := i + len(im.Label)
		parts = append(parts, llm.TextPart(rest[:cut]), part)
		rest = rest[cut:]
	}
	if rest != "" {
		parts = append(parts, llm.TextPart(rest))
	}
	return append(parts, trailing...), nil
}

// ── probe record ──

// visionMu serializes the read-modify-write of the probe record within one
// process; two engines racing on it lose at most one record, which the next
// image probes again.
var visionMu sync.Mutex

type visionRecord struct {
	Vision  bool   `json:"vision"`
	Checked string `json:"checked"`          // RFC 3339
	Detail  string `json:"detail,omitempty"` // the refusal, when false
}

func (v VisionConfig) stateFile() string {
	if v.StateFile != "" {
		return v.StateFile
	}
	return filepath.Join(DunHome(), "vision.json")
}

func readVision(file string) map[string]visionRecord {
	m := map[string]visionRecord{}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// lookupVision reports what the record says about the model: known=false
// means it has not been probed.
func (v VisionConfig) lookup() (known, vision bool) {
	if v.Key == "" {
		return false, false
	}
	visionMu.Lock()
	defer visionMu.Unlock()
	r, ok := readVision(v.stateFile())[v.Key]
	return ok, r.Vision
}

// record keeps a probe result. Best-effort: a record that cannot be written
// means the next image probes again.
func (v VisionConfig) record(vision bool, detail string) {
	if v.Key == "" {
		return
	}
	visionMu.Lock()
	defer visionMu.Unlock()
	file := v.stateFile()
	m := readVision(file)
	m[v.Key] = visionRecord{Vision: vision, Checked: time.Now().UTC().Format(time.RFC3339), Detail: detail}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(file), 0o755) != nil {
		return
	}
	tmp := file + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		_ = os.Rename(tmp, file)
	}
}

// refusesImages reports whether a provider error is the model refusing
// image input.
//
// No provider has a code for it; they say it in the body. Seen:
// OpenAI 400 "Invalid content type. image_url is only supported by certain
// models", vLLM 400 "… is not a multimodal model", OpenRouter 404 "No
// endpoints found that support image input", llama.cpp without a projector
// 500 "image input is not supported - hint: … provide the mmproj". So: a
// 4xx (not 429) whose body mentions images, vision, multimodal or the
// content type, or a 5xx with llama.cpp's own words. A 4xx about something
// else that happens to mention an image also lands here; the cost is one
// model recorded without vision, which a removed vision.json entry undoes.
func refusesImages(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	m := statusRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	code, _ := strconv.Atoi(m[1])
	switch {
	case code >= 400 && code < 500 && code != 429:
		for _, w := range []string{"image", "vision", "multimodal", "multi-modal", "content type", "content_type"} {
			if strings.Contains(s, w) {
				return true
			}
		}
	case code >= 500:
		return strings.Contains(s, "image input is not supported") || strings.Contains(s, "mmproj")
	}
	return false
}

var statusRe = regexp.MustCompile(`status (\d{3})`)

// ── describing ──

const describeInstruction = `Describe this image for an assistant who cannot see it. The user attached it to the message below; the assistant will act on your description alone. Give what it needs for that: any text in the image verbatim (code, errors, logs, labels), what the image shows and how it is laid out, and numbers or data exactly. Plain text, no preamble.

The user's message:
`

// describeImages replaces each image in the message with a description
// from the vision model: the label becomes "[image 1: a.png 640×480 —
// description: …]". The result is plain text, and it is what the session
// records, so a resume does not describe again.
func (h *Harness) describeImages(ctx context.Context, task string, imgs []ImageInput, parts []llm.ContentPart) (string, error) {
	v := h.cfg.Vision
	if v.Describer == nil {
		return "", &ImageRefusedError{Msg: noVisionMsg(v.Model)}
	}
	var images []llm.ContentPart
	for _, p := range parts {
		if p.Type == "image_url" {
			images = append(images, p)
		}
	}
	out := task
	for i, im := range imgs {
		if i >= len(images) {
			break
		}
		desc, err := h.describeOne(ctx, task, images[i])
		if err != nil {
			return "", &ImageRefusedError{Msg: fmt.Sprintf("%s: the vision model %s could not describe it: %v — the message was not sent",
				im.Label, v.DescriberModel, err)}
		}
		with := strings.TrimSuffix(im.Label, "]") + " — description: " + desc + "]"
		if im.Label != "" && strings.Contains(out, im.Label) {
			out = strings.Replace(out, im.Label, with, 1)
		} else {
			out += "\n" + with
		}
	}
	return out, nil
}

func (h *Harness) describeOne(ctx context.Context, task string, img llm.ContentPart) (string, error) {
	start := time.Now()
	msgs := []llm.Message{{Role: "user", Parts: []llm.ContentPart{llm.TextPart(describeInstruction + task), img}}}
	ch, err := h.cfg.Vision.Describer.ChatStream(ctx, msgs, nil, nil)
	if err != nil {
		h.noteSideCall("vision", start, nil)
		return "", err
	}
	var b strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("vision", start, nil)
			return "", errors.New(c.Error)
		}
		b.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("vision", start, usage)
	desc := strings.Join(strings.Fields(b.String()), " ")
	if desc == "" {
		return "", errors.New("an empty description")
	}
	return desc, nil
}

// ── the turn ──

// ImagesRefused says, before anything is sent, that a message with images
// would be refused: the model is recorded without vision and no vision
// model is configured. "" when it may go.
func (h *Harness) ImagesRefused() string {
	v := h.cfg.Vision
	if known, vision := v.lookup(); known && !vision && v.Describer == nil {
		return noVisionMsg(v.Model)
	}
	return ""
}

// AskImages is Ask for a message with images (see the top of this file).
// An *ImageRefusedError means the message was not sent and is not in the
// session; the caller hands it back to the user.
func (h *Harness) AskImages(ctx context.Context, task string, imgs []ImageInput) (agent.TurnResult, error) {
	if len(imgs) == 0 {
		return h.Ask(ctx, task)
	}
	parts, err := imageParts(task, imgs)
	if err != nil {
		return agent.TurnResult{}, err
	}
	v := h.cfg.Vision
	known, vision := v.lookup()
	if known && !vision {
		text, err := h.describeImages(ctx, task, imgs, parts)
		if err != nil {
			return agent.TurnResult{}, err
		}
		return h.Ask(ctx, text)
	}
	ctx, end := h.interruptible(ctx)
	defer end()
	h.clearBudgetNotes()
	h.prepareTurn(ctx)
	e := agent.Entry{ID: uuid.New().String(), Kind: agent.KindUser, Content: task, Parts: parts,
		CreatedAt: time.Now().UnixNano()}
	h.store.publish(e)
	res, err := h.turnThenTriggers(ctx)
	if !known {
		switch {
		case err == nil:
			v.record(true, "")
		case refusesImages(err):
			v.record(false, oneLineErr(err))
			text, derr := h.describeImages(ctx, task, imgs, parts)
			if derr != nil {
				h.store.dropEntry(e.ID)
				return res, derr
			}
			h.store.setText(e.ID, text)
			res, err = h.turnThenTriggers(ctx)
		}
	}
	return h.finishInterrupt(ctx, res, err)
}

// oneLineErr is an error's text on one line, bounded, for the probe record.
func oneLineErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
