package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // decoders, for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/iodesystems/dun/proto"
	_ "golang.org/x/image/webp"
)

// imageinput.go — images into the composer.
//
// Two ways in: ctrl+v with an image on the clipboard, and a paste whose
// whole text is the path of an image file (what a terminal pastes when a
// file is dropped on it). Either becomes an IMAGE TOKEN: one rune in the
// buffer like a paste token, drawn as its label, "[image 1: shot.png
// 1920×1080]". Value gives the label, so the label is what the message's
// text holds; Images gives the files, which the engine reads.
//
// A clipboard image has no file, so it is written to $DUN_HOME/images/ by
// content hash: the engine reads it from there, on this machine.
//
// Labels are numbered for the life of the TUI, and every label made is
// remembered (imageRegistry): text that holds one — a recalled history
// entry, a draft the engine handed back — becomes the token again.

// imageExts are the file types a pasted path is taken as an image for.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// imageRegistry is every image label this TUI made, by label. A pointer in
// multilineInput, so the copies Bubble Tea and keepDraft make share it.
type imageRegistry struct {
	n       int
	byLabel map[string]proto.Image
}

// imagePathFromPaste is the image file a paste names, if its whole text is
// one: surrounding space and quotes trimmed, a file:// URL decoded, ~
// expanded, backslash-escaped characters (a dropped "my\ shot.png")
// unescaped. The file must exist, be a regular file, and have an image
// extension.
func imagePathFromPaste(text string) (string, bool) {
	s := strings.TrimSpace(text)
	if s == "" || strings.ContainsAny(s, "\n\r") || len(s) > 4096 {
		return "", false
	}
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return "", false
		}
		s = u.Path
	} else if strings.Contains(s, `\`) {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		s = b.String()
	}
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		s = filepath.Join(home, s[1:])
	}
	if !imageExts[strings.ToLower(filepath.Ext(s))] {
		return "", false
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return "", false
	}
	if st, err := os.Stat(abs); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return abs, true
}

// imageSize reads an image file's dimensions without decoding it.
func imageSize(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return c.Width, c.Height, nil
}

// clipboardRun runs one clipboard command and returns its stdout. A var so
// a test can stand in for wl-paste and xclip.
var clipboardRun = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// clipboardCmds are tried in order for a PNG on the clipboard: Wayland,
// then X11. A command that is missing or finds no image fails, and the
// next is tried.
var clipboardCmds = [][]string{
	{"wl-paste", "--no-newline", "--type", "image/png"},
	{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// clipboardImage returns the PNG on the clipboard, or false when there is
// none (or no tool to read it with).
func clipboardImage() ([]byte, bool) {
	for _, c := range clipboardCmds {
		out, err := clipboardRun(c[0], c[1:]...)
		if err == nil && bytes.HasPrefix(out, pngMagic) {
			return out, true
		}
	}
	return nil, false
}

// saveClipboardImage writes clipboard bytes to $DUN_HOME/images/<sha>.png
// and returns the path. Content-addressed: the same image pasted twice is
// one file.
func saveClipboardImage(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	dir := filepath.Join(dunHome(), "images")
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".png")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// clipImageMsg is ctrl+v's answer: an image file to insert, or ok=false
// for no image on the clipboard (ctrl+v then does nothing, as before).
type clipImageMsg struct {
	path, name string
	w, h       int
	ok         bool
	err        error
}

// pasteImageCmd reads the clipboard off the UI goroutine.
func pasteImageCmd() tea.Cmd {
	return func() tea.Msg {
		data, ok := clipboardImage()
		if !ok {
			return clipImageMsg{}
		}
		c, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return clipImageMsg{err: fmt.Errorf("the clipboard image does not decode: %w", err)}
		}
		path, err := saveClipboardImage(data)
		if err != nil {
			return clipImageMsg{err: fmt.Errorf("could not keep the clipboard image: %w", err)}
		}
		return clipImageMsg{path: path, name: "clipboard.png", w: c.Width, h: c.Height, ok: true}
	}
}

// ── tokens (see multilineInput) ──

// addImage registers an image and returns its token.
func (m *multilineInput) addImage(path, name string, w, h int) rune {
	if m.imgs == nil {
		m.imgs = &imageRegistry{}
	}
	if m.imgs.byLabel == nil {
		m.imgs.byLabel = map[string]proto.Image{}
	}
	m.imgs.n++
	label := fmt.Sprintf("[image %d: %s %d×%d]", m.imgs.n, name, w, h)
	img := proto.Image{Path: path, Label: label}
	m.imgs.byLabel[label] = img
	return m.imageTok(img)
}

// imageTok files a registered image in the side table and returns its token.
func (m *multilineInput) imageTok(img proto.Image) rune {
	if m.images == nil {
		m.images = map[rune]proto.Image{}
	}
	if m.nextTok < tokFirst || m.nextTok > tokLast {
		m.nextTok = tokFirst
	}
	tok := m.nextTok
	m.nextTok++
	m.images[tok] = img
	return tok
}

// InsertImage puts an image token at the cursor.
func (m *multilineInput) InsertImage(path, name string, w, h int) {
	m.insert([]rune{m.addImage(path, name, w, h)})
}

// pastedImage is the image token for a paste that names an image file, or
// false. A file that does not decode is not taken as an image: its path is
// pasted as text.
func (m *multilineInput) pastedImage(text []rune) (rune, bool) {
	path, ok := imagePathFromPaste(string(text))
	if !ok {
		return 0, false
	}
	w, h, err := imageSize(path)
	if err != nil {
		return 0, false
	}
	return m.addImage(path, filepath.Base(path), w, h), true
}

// Images are the images in the buffer, in order, each once.
func (m *multilineInput) Images() []proto.Image {
	if len(m.images) == 0 {
		return nil
	}
	var out []proto.Image
	seen := map[string]bool{}
	for _, r := range m.buf {
		if img, ok := m.images[r]; ok && !seen[img.Label] {
			seen[img.Label] = true
			out = append(out, img)
		}
	}
	return out
}

// Remember registers images this TUI did not make (a draft the engine
// handed back from another client), so SetValue turns their labels into
// tokens.
func (m *multilineInput) Remember(imgs []proto.Image) {
	if len(imgs) == 0 {
		return
	}
	if m.imgs == nil {
		m.imgs = &imageRegistry{}
	}
	if m.imgs.byLabel == nil {
		m.imgs.byLabel = map[string]proto.Image{}
	}
	for _, img := range imgs {
		m.imgs.byLabel[img.Label] = img
	}
}

// tokenizeImages turns every registered label in r into its image token.
func (m *multilineInput) tokenizeImages(r []rune) []rune {
	if m.imgs == nil || len(m.imgs.byLabel) == 0 {
		return r
	}
	s := string(r)
	if !strings.Contains(s, "[image ") {
		return r
	}
	var out []rune
	for {
		i := strings.Index(s, "[image ")
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i:], ']')
		if j < 0 {
			break
		}
		label := s[i : i+j+1]
		img, ok := m.imgs.byLabel[label]
		if !ok {
			out = append(out, []rune(s[:i+1])...)
			s = s[i+1:]
			continue
		}
		out = append(out, []rune(s[:i])...)
		out = append(out, m.imageTok(img))
		s = s[i+j+1:]
	}
	return append(out, []rune(s)...)
}
