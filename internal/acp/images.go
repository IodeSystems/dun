package acp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // decoders, for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
	_ "golang.org/x/image/webp"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/proto"
)

// images.go — image blocks in a prompt (docs/acp.md, "Prompts").
//
// An image reaches dun as the TUI's does (internal/tui/imageinput.go): a
// FILE on this machine plus a label in the text, "[image 1: shot.png
// 1920×1080]", sent as proto.User.Images. The bytes of a block are written
// to $DUN_HOME/images/<sha256>.<ext>, the TUI's place for a pasted image. A
// file:// uri inside the session's cwd is used where it is. Whether the
// model reads the image, has it described, or refuses the message is the
// engine's (harness/images.go); a refusal comes back as proto.ImageRefused.

// imageMax is the most bytes of one image the bridge takes. The TUI has no
// cap; the engine scales an image to 1568 px before it sends it, so this
// only bounds what the bridge decodes and writes for a client.
const imageMax = 20 << 20

// imageExt is the file extension for each format image.DecodeConfig names.
var imageExt = map[string]string{"png": ".png", "jpeg": ".jpg", "gif": ".gif", "webp": ".webp"}

// imageBlock is one image block as the text that stands for it and, when
// the image is usable, the image. n is the count of images the session has
// numbered; a usable image takes the next number. An unusable one is a note
// in the text and no image.
func imageBlock(b *sdk.ContentBlockImage, cwd string, n *int) (string, *proto.Image) {
	var data []byte
	var path, name string
	if b.Uri != nil {
		name = uriName(*b.Uri)
	}
	tooBig := fmt.Sprintf("larger than the %d MiB limit", imageMax>>20)
	switch {
	case b.Data != "":
		if base64.StdEncoding.DecodedLen(len(b.Data)) > imageMax+3 {
			return imageNote(name, tooBig), nil
		}
		raw, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			return imageNote(name, "its data is not valid base64"), nil
		}
		data = raw
	case b.Uri != nil && strings.HasPrefix(*b.Uri, "file:"):
		p, ok := fileInCwd(*b.Uri, cwd)
		if !ok {
			return imageNote(name, "a file outside the session's folder is not read"), nil
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return imageNote(name, "the file cannot be read"), nil
		}
		if st.Size() > imageMax {
			return imageNote(name, tooBig), nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return imageNote(name, "the file cannot be read"), nil
		}
		data, path = raw, p
	case b.Uri != nil:
		return imageNote(name, "dun does not fetch "+uriScheme(*b.Uri)+" images; send the data or a file"), nil
	default:
		return imageNote(name, "it has no data"), nil
	}
	if len(data) > imageMax {
		return imageNote(name, tooBig), nil
	}
	c, format, err := image.DecodeConfig(bytes.NewReader(data))
	ext := imageExt[format]
	if err != nil || ext == "" {
		return imageNote(name, "not an image dun can read (png, jpeg, gif, webp)"), nil
	}
	if path == "" {
		sum := sha256.Sum256(data)
		path = filepath.Join(dunconfig.DunHome(), "images", hex.EncodeToString(sum[:])+ext)
		if _, err := os.Stat(path); err != nil {
			if err := fsutil.WriteAtomic(path, data, 0o644); err != nil {
				return imageNote(name, "it could not be kept: "+err.Error()), nil
			}
		}
	}
	if name == "" {
		name = "image" + ext
	}
	*n++
	label := fmt.Sprintf("[image %d: %s %d×%d]", *n, name, c.Width, c.Height)
	return label, &proto.Image{Path: path, Label: label}
}

// imageNote is the text for an image that was not passed on.
func imageNote(name, why string) string {
	if name == "" {
		name = "an image"
	}
	return "[" + name + " was not attached: " + why + "]"
}

// fileInCwd is the path of a file: uri when it is inside cwd.
func fileInCwd(raw, cwd string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || u.Path == "" || cwd == "" {
		return "", false
	}
	p := filepath.Clean(u.Path)
	rel, err := filepath.Rel(filepath.Clean(cwd), p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

// uriName is the last element of a uri's path, fit for a label ("" when
// there is none).
func uriName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	n := filepath.Base(u.Path)
	if n == "." || n == "/" || n == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r == '[' || r == ']' || r < ' ' {
			return '_'
		}
		return r
	}, n)
}

func uriScheme(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "remote"
}
