package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// toolmedia.go — media parts in a tool result (llm.ToolResult.Parts,
// agentkit v0.42.0).
//
// A tool result may carry media: an image, a recording, a video or a PDF
// that file_read returned (agentkit's files; MCP tools return text only).
// agentkit persists the parts on the result's entry and sends them where
// the endpoint takes them. dun has two jobs here, and the read policy is in
// mediapolicy.go:
//
//   - The model. A model recorded without vision (images.go, vision.json)
//     gets each image as a description from the vision model, the same
//     describer a user message's image uses. With none configured, the
//     image is left out and a label says why (the read policy already has
//     images off then; this covers a part that got through). It is
//     Session.PrepareToolResult, so it sees every toolset. A model not yet
//     probed gets the image: the probe is a user message's.
//   - The person. Every text-only view (the tool's end event, the history,
//     ACP) shows a label where an image was, never its base64.

// describeToolInstruction is the describer's prompt for an image a tool
// returned; the tool's name and the result's text follow it.
const describeToolInstruction = `Describe this image for an assistant who cannot see it. A tool the assistant called returned it; the assistant will act on your description alone. Give what it needs for that: any text in the image verbatim (code, errors, logs, labels), what the image shows and how it is laid out, and numbers or data exactly. Plain text, no preamble.

The tool, and the text it returned with the image:
`

// mediaLabel is the text that stands for a media part where only text
// shows: "[image/png, 412 KiB]". A text part has none.
func mediaLabel(p llm.ContentPart) string {
	if p.Type == "text" {
		return ""
	}
	mime, size := partMedia(p)
	if size <= 0 {
		return "[" + mime + "]"
	}
	return "[" + mime + ", " + byteSize(size) + "]"
}

// partMedia is a media part's type and its size in bytes (0 when unknown:
// a URL, not inline data).
func partMedia(p llm.ContentPart) (mime string, size int) {
	var uri string
	switch {
	case p.ImageURL != nil:
		mime, uri = "image", p.ImageURL.URL
	case p.VideoURL != nil:
		mime, uri = "video", p.VideoURL.URL
	case p.InputAudio != nil:
		mime = "audio"
		if f := strings.TrimSpace(p.InputAudio.Format); f != "" {
			mime = "audio/" + strings.ToLower(f)
		}
		return mime, len(p.InputAudio.Data) * 3 / 4
	case p.File != nil:
		mime, uri = "file", p.File.FileData
		if p.File.Filename != "" {
			mime = "file " + p.File.Filename
		}
	default:
		return p.Type, 0
	}
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return mime, 0
	}
	meta, b64, ok := strings.Cut(rest, ",")
	if !ok {
		return mime, 0
	}
	if t, _, _ := strings.Cut(meta, ";"); t != "" && (p.File == nil || p.File.Filename == "") {
		mime = strings.ToLower(t)
	}
	return mime, len(b64) * 3 / 4
}

// byteSize is n bytes for a label: 812 B, 412 KiB, 3.1 MiB.
func byteSize(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		if n%(1<<20) == 0 {
			return fmt.Sprintf("%d MiB", n>>20)
		}
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

// withMediaLabels is text, then a label per media part, one per line.
func withMediaLabels(text string, parts []llm.ContentPart) string {
	var labels []string
	for _, p := range parts {
		if l := mediaLabel(p); l != "" {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return strings.Join(labels, "\n")
	}
	return text + "\n" + strings.Join(labels, "\n")
}

// resultShown is a tool result as a text-only view shows it.
func resultShown(r llm.ToolResult) string { return withMediaLabels(r.Text, r.Parts) }

// prepareToolResult is Session.PrepareToolResult: for a model recorded
// without vision, each image part becomes text (see the top of this file).
// Other parts, and every part for a model that reads images or is not yet
// probed, pass unchanged.
func (h *Harness) prepareToolResult(ctx context.Context, tc llm.ToolCall, r llm.ToolResult) (llm.ToolResult, error) {
	if !hasImagePart(r.Parts) {
		return r, nil
	}
	if known, vision := h.cfg.Vision.lookup(); !known || vision {
		return r, nil
	}
	kept := make([]llm.ContentPart, 0, len(r.Parts))
	var stand []string
	for _, p := range r.Parts {
		if p.Type != "image_url" {
			kept = append(kept, p)
			continue
		}
		stand = append(stand, h.imageStandIn(ctx, tc.Function.Name, r.Text, p))
	}
	out := llm.ToolResult{Text: r.Text, Parts: kept}
	if len(kept) == 0 {
		out.Parts = nil
	}
	if strings.TrimSpace(out.Text) == "" {
		out.Text = strings.Join(stand, "\n")
	} else {
		out.Text += "\n" + strings.Join(stand, "\n")
	}
	return out, nil
}

func hasImagePart(parts []llm.ContentPart) bool {
	for _, p := range parts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

// imageStandIn is the text that replaces one image a tool returned, for a
// model that cannot read images: the vision model's description, or a
// label saying the image was left out and why.
func (h *Harness) imageStandIn(ctx context.Context, tool, text string, img llm.ContentPart) string {
	mime, _ := partMedia(img)
	v := h.cfg.Vision
	model := v.Model
	if model == "" {
		model = "this model"
	}
	if v.Describer == nil {
		return "[" + mime + " left out: " + model + " cannot read images, and no vision model is set to describe them]"
	}
	desc, err := h.describeOne(ctx, describeToolInstruction+tool+"\n"+oneLine(text, 2000), img)
	if err != nil {
		return "[" + mime + " left out: " + model + " cannot read images, and the vision model " + v.DescriberModel +
			" could not describe it: " + oneLineErr(err) + "]"
	}
	return "[" + mime + " from " + tool + " — description: " + desc + "]"
}
