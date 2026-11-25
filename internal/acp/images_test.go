package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imgBlock(data []byte, mime string, uri string) sdk.ContentBlock {
	b := sdk.ImageBlock(base64.StdEncoding.EncodeToString(data), mime)
	if uri != "" {
		b.Image.Uri = &uri
	}
	return b
}

func (r *rig) promptBlocks(sid sdk.SessionId, blocks ...sdk.ContentBlock) chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		res, err := r.cc.Prompt(context.Background(), sdk.PromptRequest{SessionId: sid, Prompt: blocks})
		ch <- promptResult{res.StopReason, err}
	}()
	return ch
}

func TestImageCapability(t *testing.T) {
	r := newRig(t)
	res, err := r.cc.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if !res.AgentCapabilities.PromptCapabilities.Image {
		t.Errorf("promptCapabilities.image is not advertised: %+v", res.AgentCapabilities.PromptCapabilities)
	}
}

// An image block is written under DUN_HOME/images by content hash, its
// label stands in the text where it was, and the message carries it.
func TestImagePrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	r := newRig(t)
	sid := r.newSession()
	data := pngBytes(t, 30, 20)
	ch := r.promptBlocks(sid, sdk.TextBlock("see"), imgBlock(data, "image/png", "file:///elsewhere/shot.png"), sdk.TextBlock("and"), imgBlock(data, "image/png", ""))
	c := r.d.next()
	u := c.Payload.(*proto.User)
	if len(u.Images) != 2 {
		t.Fatalf("images: %+v", u.Images)
	}
	want1, want2 := "[image 1: shot.png 30×20]", "[image 2: image.png 30×20]"
	if u.Images[0].Label != want1 || u.Images[1].Label != want2 {
		t.Errorf("labels: %+v", u.Images)
	}
	if u.Content != "see\n\n"+want1+"\n\nand\n\n"+want2 {
		t.Errorf("content: %q", u.Content)
	}
	for _, im := range u.Images {
		if filepath.Dir(im.Path) != filepath.Join(home, "images") || !strings.HasSuffix(im.Path, ".png") {
			t.Errorf("path %s", im.Path)
		}
		got, err := os.ReadFile(im.Path)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("file %s: %v", im.Path, err)
		}
	}
	if u.Images[0].Path != u.Images[1].Path {
		t.Errorf("same bytes, two files: %s %s", u.Images[0].Path, u.Images[1].Path)
	}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: u.Content}, &proto.Turn{Kind: "user"}, &proto.Done{})
	if res := waitPrompt(t, ch); res.err != nil || res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
	// Numbering goes on across the session's prompts.
	ch = r.promptBlocks(sid, sdk.TextBlock("again"), imgBlock(data, "image/png", ""))
	u = r.d.next().Payload.(*proto.User)
	if len(u.Images) != 1 || !strings.HasPrefix(u.Images[0].Label, "[image 3: ") {
		t.Errorf("second prompt: %+v", u.Images)
	}
	r.d.emit(&proto.UserEcho{ID: "m2", Content: u.Content}, &proto.Turn{Kind: "user"}, &proto.Done{})
	waitPrompt(t, ch)
}

// A file: uri inside the cwd is used where it is.
func TestImageFileInCwd(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	cwd := t.TempDir()
	p := filepath.Join(cwd, "a.png")
	if err := os.WriteFile(p, pngBytes(t, 4, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	n := 0
	text, imgs := promptParts([]sdk.ContentBlock{{Image: &sdk.ContentBlockImage{Type: "image", Uri: ptr("file://" + p)}}}, cwd, &n)
	if len(imgs) != 1 || imgs[0].Path != p || text != "[image 1: a.png 4×5]" {
		t.Errorf("%q %+v", text, imgs)
	}
	// Outside the cwd: a note.
	out := filepath.Join(t.TempDir(), "b.png")
	if err := os.WriteFile(out, pngBytes(t, 4, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	text, imgs = promptParts([]sdk.ContentBlock{{Image: &sdk.ContentBlockImage{Type: "image", Uri: ptr("file://" + out)}}}, cwd, &n)
	if len(imgs) != 0 || !strings.Contains(text, "was not attached") {
		t.Errorf("%q %+v", text, imgs)
	}
}


// Bad input is a note in the text, not an image and not a crash.
func TestImageBad(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	n := 0
	for name, b := range map[string]*sdk.ContentBlockImage{
		"base64":       {Type: "image", Data: "!!!not base64", MimeType: "image/png"},
		"type":         {Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("GIF? no, text")), MimeType: "image/bmp"},
		"http":         {Type: "image", MimeType: "image/png", Uri: ptr("https://x.test/a.png")},
		"nothing":      {Type: "image", MimeType: "image/png"},
		"too big":      {Type: "image", Data: strings.Repeat("A", (imageMax+1)*4/3+8), MimeType: "image/png"},
		"truncated":    {Type: "image", Data: base64.StdEncoding.EncodeToString(pngBytes(t, 9, 9)[:20]), MimeType: "image/png"},
		"bad file uri": {Type: "image", Uri: ptr("file:///no/such.png")},
	} {
		text, imgs := promptParts([]sdk.ContentBlock{sdk.TextBlock("x"), {Image: b}}, t.TempDir(), &n)
		if len(imgs) != 0 || !strings.Contains(text, "was not attached") || n != 0 {
			t.Errorf("%s: %q %+v n=%d", name, text, imgs, n)
		}
	}
}

// A refusal (a model with no vision and no describer) shows its message and
// ends the prompt, with no turn.
func TestImageRefused(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := newRig(t)
	sid := r.newSession()
	r.ed.take()
	ch := r.promptBlocks(sid, sdk.TextBlock("see"), imgBlock(pngBytes(t, 3, 3), "image/png", ""))
	u := r.d.next().Payload.(*proto.User)
	r.d.emit(&proto.ImageRefused{Message: "m cannot read images", Content: u.Content, Images: u.Images}, &proto.Done{})
	if res := waitPrompt(t, ch); res.err != nil || res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
	var seen bool
	for _, up := range r.ed.take() {
		if up.AgentMessageChunk != nil && strings.Contains(up.AgentMessageChunk.Content.Text.Text, "cannot read images") {
			seen = true
		}
	}
	if !seen {
		t.Error("the refusal was not shown")
	}
}

func TestAudioNote(t *testing.T) {
	text := promptText([]sdk.ContentBlock{sdk.TextBlock("a"), sdk.AudioBlock("AAAA", "audio/wav")})
	if !strings.Contains(text, "audio was attached") {
		t.Errorf("%q", text)
	}
}
