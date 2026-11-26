package harness

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"
)

// Images are off for a model recorded without vision and with no vision
// model; on with a describer, with vision, or before a probe. /media images
// send wins over the record.
func TestMediaPolicyFollowsVision(t *testing.T) {
	for _, c := range []struct {
		name      string
		known     bool
		vision    bool
		describer bool
		want      filetools.MediaMode
	}{
		{"no vision, no describer", true, false, false, filetools.MediaOff},
		{"no vision, a describer", true, false, true, ""},
		{"vision", true, true, false, ""},
		{"not probed", false, false, false, ""},
	} {
		h := newNoteHarness(t)
		h.cfg.Vision = VisionConfig{Key: "k", Model: "m", StateFile: filepath.Join(t.TempDir(), "vision.json")}
		if c.describer {
			h.cfg.Vision.Describer = &fakeDescriber{}
		}
		if c.known {
			h.cfg.Vision.record(c.vision, "")
		}
		if got := h.mediaPolicy().Image.Mode; got != c.want {
			t.Errorf("%s: images %q, want %q", c.name, got, c.want)
		}
	}
	// The record changes mid-session (a user image's probe): the next
	// policy has it.
	h := visionHarness(t, true, nil)
	if h.mediaPolicy().Image.Mode != "" {
		t.Fatal("vision: images not sent")
	}
	h.cfg.Vision.record(false, "refused")
	if h.mediaPolicy().Image.Mode != filetools.MediaOff {
		t.Error("the record now says no vision: images still sent")
	}
	if _, err := h.MediaCmd("images send"); err != nil {
		t.Fatal(err)
	}
	if h.mediaPolicy().Image.Mode != filetools.MediaSend {
		t.Error("/media images send did not win over the record")
	}
}

// /media sets each part of the policy for the session.
func TestMediaCmdSetsPolicy(t *testing.T) {
	h := visionHarness(t, true, nil)
	steps := []struct {
		cmd   string
		check func(filetools.MediaPolicy) bool
	}{
		{"cap 20MB", func(p filetools.MediaPolicy) bool {
			return p.Image.MaxBytes == 20<<20 && p.Video.MaxBytes == 20<<20 && p.PDF.MaxBytes == 20<<20
		}},
		{"cap video 1.5GB", func(p filetools.MediaPolicy) bool { return p.Video.MaxBytes == 3<<29 && p.Image.MaxBytes == 20<<20 }},
		{"cap default", func(p filetools.MediaPolicy) bool { return p.Image.MaxBytes == 0 && p.Video.MaxBytes == 0 }},
		{"audio off", func(p filetools.MediaPolicy) bool { return p.Audio.Mode == filetools.MediaOff }},
		{"audio auto", func(p filetools.MediaPolicy) bool { return p.Audio.Mode == "" }},
		{"scaler none", func(p filetools.MediaPolicy) bool { return p.Tools.None && p.Scaler != nil }},
		{"scale off", func(p filetools.MediaPolicy) bool { return p.Scaler != nil }},
	}
	for _, s := range steps {
		if _, err := h.MediaCmd(s.cmd); err != nil {
			t.Fatalf("%s: %v", s.cmd, err)
		}
		if !s.check(h.mediaPolicy()) {
			t.Errorf("%s: policy %+v", s.cmd, h.mediaPolicy())
		}
	}
	if _, err := h.mediaPolicy().Scaler(context.Background(), filetools.MediaIn{}); err != errScaleOff {
		t.Errorf("scale off: the scaler says %v", err)
	}
	// A pinned scaler goes to its tool's field.
	dir := t.TempDir()
	magick := filepath.Join(dir, "magick")
	if err := os.WriteFile(magick, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = h.MediaCmd("scale on")
	if _, err := h.MediaCmd("scaler " + magick); err != nil {
		t.Fatal(err)
	}
	if p := h.mediaPolicy(); p.Tools.Magick != magick || p.Scaler != nil {
		t.Errorf("scaler pinned: %+v", p.Tools)
	}
	out, _ := h.MediaCmd("")
	for _, want := range []string{"image: send", "cap 5 MiB", "scaler: " + magick, "scale: on"} {
		if !strings.Contains(out, want) {
			t.Errorf("/media is missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"images maybe", "cap lots", "scaler /nope/magick", "scale sideways", "frobnicate"} {
		if _, err := h.MediaCmd(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// dunScaler shrinks a WebP, which agentkit's stdlib scaler cannot decode.
func TestDunScalerWebP(t *testing.T) {
	abs, _ := filepath.Abs("testdata/media.webp")
	st, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dunScaler(context.Background(), filetools.MediaIn{Path: abs, MIME: "image/webp", Kind: filetools.MediaImage,
		Bytes: st.Size(), Width: 150, Height: 100, MaxBytes: 1 << 20, MaxLongEdge: 60})
	if err != nil {
		t.Fatal(err)
	}
	c, format, err := image.DecodeConfig(bytes.NewReader(out.Data))
	if err != nil || out.MIME != "image/jpeg" || format != "jpeg" || c.Width != 60 || c.Height != 40 {
		t.Errorf("%s %s %dx%d %v", out.MIME, format, c.Width, c.Height, err)
	}
	if _, err := dunScaler(context.Background(), filetools.MediaIn{Kind: filetools.MediaVideo, MIME: "video/mp4"}); err == nil {
		t.Error("a video: no error")
	}
}

// A read of an image over the edge, through withFiles with dun's scaler:
// the model gets the scaled copy, the person a note, the ledger a media line.
func TestReadImageScaledNoteAndEvent(t *testing.T) {
	h, ts, _ := editHarness(t)
	dir := t.TempDir()
	h.rounds.ledger = NewStatsLedger(dir)
	h.cfg.Vision = VisionConfig{Key: "k", StateFile: filepath.Join(t.TempDir(), "vision.json")}
	var notes []string
	h.cfg.OnMediaNote = func(s string) { notes = append(notes, s) }
	cfg, _ := h.filesConfig()
	cfg.Tools = []string{filetools.Read}
	ts, err := filetools.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2000, 40))
	for x := 0; x < 2000; x++ {
		for y := 0; y < 40; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	if err := os.WriteFile(filepath.Join(h.cfg.Workspace, "wide.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// External tools off: dun's scaler, whatever is on PATH.
	if _, err := h.MediaCmd("scaler none"); err != nil {
		t.Fatal(err)
	}
	d := withFiles(func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.TextResult("not mine"), nil },
		ts, nil, h, h.readWhole, nil)
	res, err := d(context.Background(), mediaCall(ts.Name(filetools.Read), `{"path":"wide.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parts) != 1 || res.Parts[0].Type != "image_url" {
		t.Fatalf("result %q, parts %v", res.Text, len(res.Parts))
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "scaled wide.png 2000→1568 px") || !strings.HasSuffix(notes[0], "(x/image)") {
		t.Errorf("notes %q", notes)
	}
	evs := ledgerEvents(t, dir)
	if len(evs) != 1 || evs[0].Kind != "media" || evs[0].Outcome != "scaled" || evs[0].Tool != "host" ||
		evs[0].Cause != "image" || evs[0].Before != buf.Len() || evs[0].After == 0 {
		t.Errorf("events %+v", evs)
	}

	// Images off (/media): no part, a label, an "off" note.
	notes = nil
	_, _ = h.MediaCmd("images off")
	res, _ = d(context.Background(), mediaCall(ts.Name(filetools.Read), `{"path":"wide.png"}`))
	if len(res.Parts) != 0 || !strings.Contains(res.Text, "media is off") {
		t.Errorf("off: %q %d parts", res.Text, len(res.Parts))
	}
	if len(notes) != 1 || notes[0] != "wide.png not sent: image is off for this session. /media to change" {
		t.Errorf("off notes %q", notes)
	}

	// Over the cap with scaling off: refused, said so.
	notes = nil
	_, _ = h.MediaCmd("images auto")
	_, _ = h.MediaCmd("cap 1KB")
	_, _ = h.MediaCmd("scale off")
	res, _ = d(context.Background(), mediaCall(ts.Name(filetools.Read), `{"path":"wide.png"}`))
	if len(res.Parts) != 0 || !strings.HasPrefix(res.Text, "ERROR: ") {
		t.Errorf("over the cap, not shrunk: %q, want an ERROR: result", res.Text)
	}
	if len(notes) != 1 || notes[0] != "wide.png over the 1 KiB limit; not scaled (scale is off). /media to change" {
		t.Errorf("refused notes %q", notes)
	}
}
