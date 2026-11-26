package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"
	xdraw "golang.org/x/image/draw"
)

// mediapolicy.go — how file_read treats media (agentkit's files.MediaPolicy).
//
// agentkit's read returns an image, a recording, a video or a PDF as a media
// part, under a per-toolset policy: per kind send or off, a byte cap (5 MiB),
// a long edge for images (1568 px), and a scaler (magick, convert, ffmpeg,
// vipsthumbnail, then the stdlib; or a host's). dun builds that policy from
// two things and sets it on the toolset before every read, so a change is in
// force at the next read on every toolset (workspace, session tmp, /file add):
//
//   - The session's /media choices (mediaSettings): a kind send|off, a cap,
//     scale on|off, a pinned scaler. Per session, not saved.
//   - The model's vision record (images.go): a model recorded without vision,
//     with no vision model to describe for it, gets images off — the file is
//     not read into the history at all. The record is read at each read, so
//     a probe that records "no vision" mid-session applies from the next one.
//     /media images send overrides it (the image then reaches
//     PrepareToolResult, which still puts a label in its place).
//
// With no external image tool on the machine, the policy's host scaler is
// dun's (dunScaler): x/image decodes WebP, which agentkit's stdlib scaler
// cannot. A host scaler replaces agentkit's tools entirely, so it is set only
// when none of them is there. Scale off is a host scaler that refuses.
//
// Each time a limit applied (OnMedia), the person gets a quiet note and the
// rounds ledger a "media" event.

// mediaSettings is the session's /media choices; the zero value is
// agentkit's defaults plus the vision rule.
type mediaSettings struct {
	mu       sync.Mutex
	mode     map[filetools.MediaKind]filetools.MediaMode
	maxBytes map[filetools.MediaKind]int64
	scaleOff bool
	tools    filetools.MediaTools // a pinned scaler, or none
}

// mediaKindNames are the kinds /media names, and agentkit's kind for each.
var mediaKindNames = map[string]filetools.MediaKind{
	"images": filetools.MediaImage, "image": filetools.MediaImage,
	"audio": filetools.MediaAudio, "video": filetools.MediaVideo,
	"pdf": filetools.MediaPDF, "pdfs": filetools.MediaPDF,
}

var mediaKindOrder = []filetools.MediaKind{filetools.MediaImage, filetools.MediaAudio, filetools.MediaVideo, filetools.MediaPDF}

// mediaPolicy is the policy in force now.
func (h *Harness) mediaPolicy() filetools.MediaPolicy {
	s := &h.media
	s.mu.Lock()
	p := filetools.MediaPolicy{Tools: s.tools}
	lims := map[filetools.MediaKind]*filetools.MediaLimit{filetools.MediaImage: &p.Image,
		filetools.MediaAudio: &p.Audio, filetools.MediaVideo: &p.Video, filetools.MediaPDF: &p.PDF}
	for k, l := range lims {
		l.Mode, l.MaxBytes = s.mode[k], s.maxBytes[k]
	}
	scaleOff := s.scaleOff
	s.mu.Unlock()
	if p.Image.Mode == "" && h.imagesUnreadable() {
		p.Image.Mode = filetools.MediaOff
	}
	switch {
	case scaleOff:
		p.Scaler = scaleRefused
	case mediaScalerTool(p.Tools) == "":
		p.Scaler = dunScaler
	}
	return p
}

// imagesUnreadable: the model is recorded without vision and nothing can
// describe an image for it.
func (h *Harness) imagesUnreadable() bool {
	v := h.cfg.Vision
	known, vision := v.lookup()
	return known && !vision && v.Describer == nil
}

// applyMediaPolicy sets the policy on ts before a read.
func (h *Harness) applyMediaPolicy(ts *filetools.Toolset) {
	if ts != nil {
		ts.SetMediaPolicy(h.mediaPolicy())
	}
}

// filesConfig is filesToolset for this session: its media events too.
func (h *Harness) filesConfig() (filetools.Config, bool) {
	cfg, ok := filesToolset(h.cfg.Files, h.cfg.Workspace)
	cfg.OnMedia = h.onMedia
	// A stubbed part's "media:<hex>" read (StubMediaOlderThan): the session's
	// entries hold it while it is in context. Read at the call: the Session
	// is built after the first toolset.
	cfg.MediaRef = func(ctx context.Context, ref string) (llm.ContentPart, bool) {
		if h.Session == nil {
			return llm.ContentPart{}, false
		}
		return h.Session.MediaRef(ctx, ref)
	}
	return cfg, ok
}

// mediaStubRounds is ShaperPolicy.StubMediaOlderThan: a media part stops
// being resent once this many model rounds have followed it. 4: the model
// looks at a read image in the round after the read and in the next few as
// it acts on it (edit, check, read again), and a screenshot is 1–2k tokens
// of image per request after that; it is a "media:<hex>" read away. agentkit
// steps the boundary by LODStep, so the prompt prefix stays cacheable.
const mediaStubRounds = 4

// setMediaStub sets the Shaper's old-media rule (agentkit mediastub.go): a
// part that mediaStubRounds model rounds have followed renders as a label
// naming its "media:<hex>", and says to call file_read with it (dun sets no
// NamePrefix, so the read tool is file_read); filesConfig reads it back.
func setMediaStub(p *agent.ShaperPolicy) {
	p.StubMediaOlderThan = mediaStubRounds
	p.MediaReadTool = filetools.DefaultPrefix + filetools.Read
}

// ── the scaler ──

// mediaToolNames is agentkit's image tool order (files/scale.go
// imageTools); convert is never used on Windows there, nor here.
var mediaToolNames = []string{"magick", "convert", "ffmpeg", "vipsthumbnail"}

var (
	mediaLookMu sync.Mutex
	mediaLook   = map[string]string{}
)

// mediaScalerTool is the external tool agentkit would scale an image with
// under tools: a pinned path, else the first on PATH; "" when none.
func mediaScalerTool(tools filetools.MediaTools) string {
	pinned := map[string]string{"magick": tools.Magick, "convert": tools.Convert,
		"ffmpeg": tools.FFmpeg, "vipsthumbnail": tools.VipsThumbnail}
	for _, n := range mediaToolNames {
		if pinned[n] != "" {
			return pinned[n]
		}
	}
	if tools.None {
		return ""
	}
	for _, n := range mediaToolNames {
		if n == "convert" && isWindows {
			continue
		}
		mediaLookMu.Lock()
		p, ok := mediaLook[n]
		if !ok {
			p, _ = exec.LookPath(n)
			mediaLook[n] = p
		}
		mediaLookMu.Unlock()
		if p != "" {
			return p
		}
	}
	return ""
}

var isWindows = filepath.Separator == '\\'

// errScaleOff is the refusal of a session with scaling off.
var errScaleOff = errors.New("scaling is off for this session (/media scale on)")

func scaleRefused(context.Context, filetools.MediaIn) (filetools.MediaOut, error) {
	return filetools.MediaOut{}, errScaleOff
}

// dunScalerPixels and dunScalerInput bound what dunScaler decodes, as
// agentkit's stdlib scaler does: decoding allocates width × height × 4.
const (
	dunScalerPixels = 40_000_000
	dunScalerInput  = 64 << 20
)

// dunScaler is the host scaler with no external tool: png, jpeg, gif and
// webp (x/image), scaled to the edge with CatmullRom and sent as JPEG, or
// PNG when it may have transparency. Audio and video need ffmpeg.
func dunScaler(_ context.Context, in filetools.MediaIn) (filetools.MediaOut, error) {
	if in.Kind != filetools.MediaImage {
		return filetools.MediaOut{}, fmt.Errorf("no ffmpeg on PATH for %s", in.MIME)
	}
	if int64(in.Width)*int64(in.Height) > dunScalerPixels {
		return filetools.MediaOut{}, fmt.Errorf("%d×%d is over %d MP", in.Width, in.Height, dunScalerPixels/1_000_000)
	}
	f, err := os.Open(in.Path)
	if err != nil {
		return filetools.MediaOut{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, dunScalerInput+1))
	_ = f.Close()
	if err != nil {
		return filetools.MediaOut{}, err
	}
	if len(raw) > dunScalerInput {
		return filetools.MediaOut{}, fmt.Errorf("over %d MiB", dunScalerInput>>20)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return filetools.MediaOut{}, fmt.Errorf("no decoder for %s (png, jpeg, gif, webp): %w", in.MIME, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > dunScalerPixels {
		return filetools.MediaOut{}, fmt.Errorf("%d×%d is over %d MP", cfg.Width, cfg.Height, dunScalerPixels/1_000_000)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return filetools.MediaOut{}, err
	}
	b := src.Bounds()
	edge, quality := in.MaxLongEdge, 85
	if edge <= 0 || max(b.Dx(), b.Dy()) < edge {
		edge = max(b.Dx(), b.Dy())
	}
	if in.Attempt > 0 {
		edge, quality = max(1, edge*7/10), 60
	}
	dst := src
	if long := max(b.Dx(), b.Dy()); long > edge {
		w, hgt := max(1, b.Dx()*edge/long), max(1, b.Dy()*edge/long)
		rgba := image.NewRGBA(image.Rect(0, 0, w, hgt))
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), src, b, xdraw.Src, nil)
		dst = rgba
	}
	var buf bytes.Buffer
	if o, ok := src.(interface{ Opaque() bool }); in.Alpha || ok && !o.Opaque() {
		if err := png.Encode(&buf, dst); err != nil {
			return filetools.MediaOut{}, err
		}
		return filetools.MediaOut{Data: buf.Bytes(), MIME: "image/png", Note: "x/image"}, nil
	}
	flat := image.NewRGBA(dst.Bounds())
	draw.Draw(flat, flat.Bounds(), dst, dst.Bounds().Min, draw.Src)
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: quality}); err != nil {
		return filetools.MediaOut{}, err
	}
	return filetools.MediaOut{Data: buf.Bytes(), MIME: "image/jpeg", Note: "x/image"}, nil
}

// ── events ──

// onMedia is the toolsets' OnMedia: a note for the person, a ledger line.
func (h *Harness) onMedia(ev filetools.MediaEvent) {
	h.noteRoundEvent(mediaRoundEvent(ev))
	if h.cfg.OnMediaNote != nil {
		h.cfg.OnMediaNote(h.mediaNote(ev))
	}
}

// mediaOutcome is the event's outcome word: scaled, unscaled (sent as it
// was, the scaler failed), off, or refused.
func mediaOutcome(ev filetools.MediaEvent) string {
	switch {
	case ev.Scaled:
		return "scaled"
	case !ev.Refused:
		return "unscaled"
	case strings.Contains(ev.Reason, "media is off"):
		return "off"
	}
	return "refused"
}

func mediaRoundEvent(ev filetools.MediaEvent) RoundEvent {
	return RoundEvent{Kind: "media", Outcome: mediaOutcome(ev), Tool: ev.Tool, Cause: string(ev.Kind), Key: ev.MIME,
		Sent: ev.OutMIME, Before: int(ev.Before.Bytes), After: int(ev.After.Bytes), Reason: oneLine(ev.Reason, 200)}
}

// mediaToolWord names the tool that made the copy for a note.
func mediaToolWord(tool string) string {
	switch tool {
	case "host":
		return "x/image"
	case "none", "":
		return "no scaler"
	}
	return tool
}

// mediaNote is the person's line for ev.
func (h *Harness) mediaNote(ev filetools.MediaEvent) string {
	name := filepath.Base(ev.Path)
	tool := mediaToolWord(ev.Tool)
	switch mediaOutcome(ev) {
	case "scaled":
		size := byteSize(int(ev.Before.Bytes)) + "→" + byteSize(int(ev.After.Bytes))
		if ev.Before.Width > 0 && ev.After.Width > 0 {
			size = fmt.Sprintf("%d→%d px, %s", max(ev.Before.Width, ev.Before.Height), max(ev.After.Width, ev.After.Height), size)
		}
		return fmt.Sprintf("scaled %s %s (%s)", name, size, tool)
	case "unscaled":
		return fmt.Sprintf("sent %s unscaled: %s", name, strings.TrimPrefix(ev.Reason, "sent unscaled: "))
	case "off":
		return fmt.Sprintf("%s not sent: %s is off for this session. /media to change", name, ev.Kind)
	}
	limit := h.mediaPolicyLimit(ev.Kind)
	if ev.Before.Bytes > limit {
		why := "could not be shrunk by " + tool
		switch {
		case strings.Contains(ev.Reason, errScaleOff.Error()):
			why = "not scaled (scale is off)"
		case ev.Tool == "none":
			why = "not scaled (no scaler)"
		}
		return fmt.Sprintf("%s over the %s limit; %s. /media to change", name, byteSize(int(limit)), why)
	}
	return fmt.Sprintf("%s not sent: %s. /media to change", name, oneLine(ev.Reason, 200))
}

// mediaPolicyLimit is the byte cap for k now.
func (h *Harness) mediaPolicyLimit(k filetools.MediaKind) int64 {
	h.media.mu.Lock()
	n := h.media.maxBytes[k]
	h.media.mu.Unlock()
	if n <= 0 {
		return filetools.DefaultMediaMaxBytes
	}
	return n
}

// ── /media ──

const mediaUsage = "usage: /media [images|audio|video|pdf send|off|auto | cap [<kind>] <size>|default | scale on|off | scaler <path>|auto|none]"

// MediaCmd is /media: alone, the policy in force; otherwise one change to
// it, for this session.
func (h *Harness) MediaCmd(action string) (string, error) {
	f := strings.Fields(action)
	if len(f) == 0 {
		return h.mediaStatus(), nil
	}
	verb := strings.ToLower(f[0])
	s := &h.media
	switch {
	case mediaKindNames[verb] != "" && len(f) == 2:
		k := mediaKindNames[verb]
		var m filetools.MediaMode
		switch strings.ToLower(f[1]) {
		case "send", "on":
			m = filetools.MediaSend
		case "off":
			m = filetools.MediaOff
		case "auto", "default":
		default:
			return "", errors.New(mediaUsage)
		}
		s.mu.Lock()
		if s.mode == nil {
			s.mode = map[filetools.MediaKind]filetools.MediaMode{}
		}
		if m == "" {
			delete(s.mode, k)
		} else {
			s.mode[k] = m
		}
		s.mu.Unlock()
	case verb == "cap" && (len(f) == 2 || len(f) == 3):
		kinds := mediaKindOrder
		arg := f[1]
		if len(f) == 3 {
			k := mediaKindNames[strings.ToLower(f[1])]
			if k == "" {
				return "", errors.New(mediaUsage)
			}
			kinds, arg = []filetools.MediaKind{k}, f[2]
		}
		n := int64(0)
		if a := strings.ToLower(arg); a != "default" && a != "auto" {
			var err error
			if n, err = parseByteSize(arg); err != nil {
				return "", err
			}
		}
		s.mu.Lock()
		if s.maxBytes == nil {
			s.maxBytes = map[filetools.MediaKind]int64{}
		}
		for _, k := range kinds {
			s.maxBytes[k] = n
		}
		s.mu.Unlock()
	case verb == "scale" && len(f) == 2 && (strings.EqualFold(f[1], "on") || strings.EqualFold(f[1], "off")):
		s.mu.Lock()
		s.scaleOff = strings.EqualFold(f[1], "off")
		s.mu.Unlock()
	case verb == "scaler" && len(f) == 2:
		var t filetools.MediaTools
		switch arg := f[1]; strings.ToLower(arg) {
		case "auto", "default":
		case "none":
			t.None = true
		default:
			if _, err := os.Stat(arg); err != nil {
				return "", fmt.Errorf("scaler %s: %v", arg, err)
			}
			switch strings.TrimSuffix(strings.ToLower(filepath.Base(arg)), ".exe") {
			case "magick":
				t.Magick = arg
			case "convert":
				t.Convert = arg
			case "ffmpeg":
				t.FFmpeg = arg
			case "vipsthumbnail":
				t.VipsThumbnail = arg
			default:
				return "", fmt.Errorf("scaler %s: name one of magick, convert, ffmpeg or vipsthumbnail", arg)
			}
		}
		s.mu.Lock()
		s.tools = t
		s.mu.Unlock()
	default:
		return "", errors.New(mediaUsage)
	}
	return h.mediaStatus(), nil
}

// parseByteSize reads 20MB, 20MiB, 500K, 5m or a plain byte count. MB and
// MiB are both 1<<20: a cap, not a disk label.
func parseByteSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		n   int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suf) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suf)), u.n
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("cap %s: a size, e.g. 20MB", s)
	}
	return int64(v * float64(mult)), nil
}

// mediaStatus is /media alone: each kind, the caps, scale and the scaler.
func (h *Harness) mediaStatus() string {
	p := h.mediaPolicy()
	h.media.mu.Lock()
	mode := map[filetools.MediaKind]filetools.MediaMode{}
	for k, m := range h.media.mode {
		mode[k] = m
	}
	scaleOff := h.media.scaleOff
	h.media.mu.Unlock()
	lims := map[filetools.MediaKind]filetools.MediaLimit{filetools.MediaImage: p.Image,
		filetools.MediaAudio: p.Audio, filetools.MediaVideo: p.Video, filetools.MediaPDF: p.PDF}
	v := h.cfg.Vision
	model := v.Model
	if model == "" {
		model = "the model"
	}
	var b strings.Builder
	b.WriteString("media file_read sends (this session):\n")
	for _, k := range mediaKindOrder {
		l := lims[k]
		m := string(l.Mode)
		if m == "" {
			m = "send"
		}
		why := ""
		if k == filetools.MediaImage {
			switch known, vision := v.lookup(); {
			case mode[k] != "":
				why = " (set by /media)"
			case known && vision:
				why = " (" + model + " reads images)"
			case known && v.Describer != nil:
				why = " (" + model + " cannot read images; " + v.DescriberModel + " describes them)"
			case known:
				why = " (" + model + " cannot read images, and no vision model is set)"
			default:
				why = " (" + model + " not yet sent an image)"
			}
		} else if mode[k] != "" {
			why = " (set by /media)"
		}
		capBytes := l.MaxBytes
		if capBytes <= 0 {
			capBytes = filetools.DefaultMediaMaxBytes
		}
		fmt.Fprintf(&b, "  %-6s %s%s, cap %s\n", string(k)+":", m, why, byteSize(int(capBytes)))
	}
	scaler := mediaScalerTool(p.Tools)
	switch {
	case scaler != "":
		scaler += " (then the stdlib for png, jpeg, gif)"
	case p.Tools.None:
		scaler = "dun's x/image (png, jpeg, gif, webp; external tools off by /media scaler none)"
	default:
		scaler = "dun's x/image (png, jpeg, gif, webp; no magick, convert, ffmpeg or vipsthumbnail on PATH)"
	}
	scale := fmt.Sprintf("on: an image over %d px or a file over its cap is shrunk", filetools.DefaultMaxLongEdge)
	if scaleOff {
		scale = "off: over a cap is not sent; over the edge is sent as is"
	}
	fmt.Fprintf(&b, "  scale: %s\n  scaler: %s\n", scale, scaler)
	b.WriteString("change: /media images off · /media cap 20MB · /media cap video 50MB · /media scale off · /media scaler /usr/bin/magick")
	return b.String()
}
