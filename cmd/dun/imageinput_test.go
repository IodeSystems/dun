package main

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

func writeImage(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := os.WriteFile(path, pngBytes(t, w, h), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A paste that is the path of an image file becomes an image token, in
// the forms a terminal pastes a dropped file in; anything else is text.
func TestComposer_PathPasteBecomesImage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shot := filepath.Join(home, "my shot.png")
	writeImage(t, shot, 32, 16)
	notImage := filepath.Join(home, "notes.txt")
	os.WriteFile(notImage, []byte("hi"), 0o644)
	fake := filepath.Join(home, "fake.png")
	os.WriteFile(fake, []byte("not a png"), 0o644)

	for _, p := range []string{
		shot,
		"'" + shot + "'",
		`"` + shot + `"` + "\n",
		strings.ReplaceAll(shot, " ", `\ `),
		"file://" + (&url.URL{Path: shot}).EscapedPath(),
		"~/my shot.png",
	} {
		in := newMultilineInput()
		in.HandleKey(paste("see "))
		in.HandleKey(paste(p))
		imgs := in.Images()
		if len(imgs) != 1 || imgs[0].Path != shot {
			t.Fatalf("%q: images %+v", p, imgs)
		}
		if want := "see [image 1: my shot.png 32×16]"; in.Value() != want || imgs[0].Label != "[image 1: my shot.png 32×16]" {
			t.Fatalf("%q: value %q", p, in.Value())
		}
		if len(in.buf) != len("see ")+1 {
			t.Fatalf("%q: the image is not one token: %d runes", p, len(in.buf))
		}
	}
	for _, p := range []string{notImage, fake, filepath.Join(home, "gone.png"), shot + " and more"} {
		in := newMultilineInput()
		in.HandleKey(paste(p))
		if len(in.Images()) != 0 || in.Value() != p {
			t.Fatalf("%q became an image: %q", p, in.Value())
		}
	}
}

// An image token behaves like a paste token: one unit to delete, kept by
// a recalled history entry, untouched by ctrl+o, gone on Reset.
func TestComposer_ImageTokenLikePasteToken(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "a.png")
	writeImage(t, shot, 4, 3)
	in := newMultilineInput()
	in.HandleKey(paste(shot))
	in.HandleKey(paste(" x"))
	sent := in.Value()
	in.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if in.Value() != sent || len(in.Images()) != 1 {
		t.Fatalf("ctrl+o opened the image: %q", in.Value())
	}
	in.Reset()
	if len(in.Images()) != 0 {
		t.Fatal("Reset kept the image")
	}
	in.SetValue(sent) // history recall
	if imgs := in.Images(); len(imgs) != 1 || imgs[0].Path != shot || in.Value() != sent {
		t.Fatalf("recall: %+v %q", imgs, in.Value())
	}
	in.CursorEnd()
	in.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	in.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	in.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if in.Value() != "" || len(in.Images()) != 0 {
		t.Fatalf("backspace over the token: %q", in.Value())
	}
}

// ctrl+v reads a PNG from the first clipboard tool that has one, keeps it
// under $DUN_HOME/images, and answers with its size; no image, no insert.
func TestClipboardImage_InjectedRunner(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	data := pngBytes(t, 20, 10)
	var tried []string
	old := clipboardRun
	defer func() { clipboardRun = old }()
	clipboardRun = func(name string, args ...string) ([]byte, error) {
		tried = append(tried, name)
		if name == "xclip" {
			return data, nil
		}
		return nil, errors.New("No suitable type of content copied")
	}
	msg := pasteImageCmd()().(clipImageMsg)
	if !msg.ok || msg.w != 20 || msg.h != 10 || strings.Join(tried, ",") != "wl-paste,xclip" {
		t.Fatalf("msg %+v, tried %v", msg, tried)
	}
	if got, err := os.ReadFile(msg.path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("kept %s: %v", msg.path, err)
	}
	if filepath.Dir(msg.path) != filepath.Join(dunHome(), "images") {
		t.Fatalf("kept outside $DUN_HOME/images: %s", msg.path)
	}

	clipboardRun = func(string, ...string) ([]byte, error) { return []byte("just text"), nil }
	if msg := pasteImageCmd()().(clipImageMsg); msg.ok || msg.err != nil {
		t.Fatalf("text on the clipboard: %+v", msg)
	}

	m := newTUIModel(&dunProc{}, "/ws")
	m.input.width = 80
	got, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	if cmd == nil {
		t.Fatal("ctrl+v read nothing")
	}
	if _, ok := cmd().(clipImageMsg); !ok {
		t.Fatal("ctrl+v did not read the clipboard")
	}
	m = got.(tuiModel)
	got, _ = m.Update(clipImageMsg{path: "/x/c.png", name: "clipboard.png", w: 20, h: 10, ok: true})
	m = got.(tuiModel)
	if v := m.input.Value(); v != "[image 1: clipboard.png 20×10]" {
		t.Fatalf("composer: %q", v)
	}
}

// The engine reads a message's images off its input, and a refusal goes
// out as image_refused then done, in shapes that decode strictly.
func TestEngine_ImagesInAndRefusalOut(t *testing.T) {
	s := newInputStreamFrom(strings.NewReader(
		`{"type":"user","content":"x [image 1: a.png 1×1]","images":[{"path":"/a.png","label":"[image 1: a.png 1×1]"}]}` + "\n"))
	msg := <-s.users
	if len(msg.images) != 1 || msg.images[0].Path != "/a.png" {
		t.Fatalf("images %+v", msg.images)
	}

	var buf bytes.Buffer
	refuseImages(&emitter{w: &buf}, "no vision", msg.content, msg.images)
	evs := decodeStrict[proto.Event](t, buf.Bytes())
	if len(evs) != 2 || evs[0].Type != "image_refused" || evs[1].Type != "done" {
		t.Fatalf("events %+v", evs)
	}
	if r := evs[0].Payload.(*proto.ImageRefused); r.Content != msg.content || len(r.Images) != 1 {
		t.Fatalf("refusal %+v", r)
	}
}

// A message the engine refused for its images comes back to the composer,
// image tokens and all, with the reason in the conversation.
func TestTUI_ImageRefusedKeepsDraft(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.input.width = 80
	label := "[image 4: s.png 8×8]"
	m = m.handleEvent(evMsg{"type": "image_refused", "message": `m cannot read images … "vision_model": …`,
		"content": "fix " + label, "images": []any{map[string]any{"path": "/p/s.png", "label": label}}})
	if v := m.input.Value(); v != "fix "+label {
		t.Fatalf("composer: %q", v)
	}
	if imgs := m.input.Images(); len(imgs) != 1 || imgs[0] != (proto.Image{Path: "/p/s.png", Label: label}) {
		t.Fatalf("images: %+v", imgs)
	}
	if !strings.Contains(convoText(m), "not sent:") || !strings.Contains(convoText(m), "vision_model") {
		t.Fatalf("no notice:\n%s", convoText(m))
	}
}
