package harness

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/fsutil"
)

// storemedia.go — media parts in the session file: blobs, never base64.
//
// Any entry's Parts — a user message's images, a tool result's image, audio,
// video or PDF (llm.ToolResult.Parts, stored as the KindToolResult entry's
// Parts) — have their inline bytes written to the session's blobs/ by
// content hash, and the part keeps a ref in their place. In memory the
// entry keeps its data, so only the on-disk form changes.
//
// Refs, NUL-led like blobMarker so no real URL is one:
//
//	"\x00dun-image:<sha>.<ext>"         an image's URL (the old form, still read)
//	"\x00dun-media:<mime> <sha>.<ext>"  a video's URL, a file's data, audio's data

const (
	imageMarker = "\x00dun-image:"
	mediaMarker = "\x00dun-media:"
)

// saveMedia returns parts with every inline media part's bytes written to
// blobs/ and replaced by a ref. A copy: the in-memory entry keeps its data.
// A part that cannot be written stays inline rather than being lost.
func (s *sessionStore) saveMedia(parts []llm.ContentPart) []llm.ContentPart {
	if len(parts) == 0 {
		return parts
	}
	out := make([]llm.ContentPart, len(parts))
	for i, p := range parts {
		out[i] = p
		switch {
		case p.ImageURL != nil:
			mime, data, ok := decodeDataURI(p.ImageURL.URL)
			if !ok || !strings.HasPrefix(mime, "image/") {
				continue
			}
			name, ok := s.writeMedia(mime, data)
			if !ok {
				continue
			}
			u := *p.ImageURL
			u.URL = imageMarker + name
			out[i].ImageURL = &u
		case p.VideoURL != nil:
			if ref, ok := s.saveDataURI(p.VideoURL.URL); ok {
				u := *p.VideoURL
				u.URL = ref
				out[i].VideoURL = &u
			}
		case p.File != nil:
			if ref, ok := s.saveDataURI(p.File.FileData); ok {
				f := *p.File
				f.FileData = ref
				out[i].File = &f
			}
		case p.InputAudio != nil:
			data, err := base64.StdEncoding.DecodeString(p.InputAudio.Data)
			if err != nil || len(data) == 0 {
				continue
			}
			mime := "audio/" + safeMediaSub(p.InputAudio.Format)
			if name, ok := s.writeMedia(mime, data); ok {
				a := *p.InputAudio
				a.Data = mediaMarker + mime + " " + name
				out[i].InputAudio = &a
			}
		}
	}
	return out
}

// saveDataURI writes a data: URI's bytes to blobs/ and returns its ref.
func (s *sessionStore) saveDataURI(u string) (string, bool) {
	mime, data, ok := decodeDataURI(u)
	if !ok {
		return "", false
	}
	name, ok := s.writeMedia(mime, data)
	if !ok {
		return "", false
	}
	return mediaMarker + mime + " " + name, true
}

// writeMedia writes data to blobs/<sha256>.<ext> once and returns its name.
func (s *sessionStore) writeMedia(mime string, data []byte) (string, bool) {
	sum := sha256.Sum256(data)
	_, sub, _ := strings.Cut(mime, "/")
	name := hex.EncodeToString(sum[:]) + "." + safeMediaSub(sub)
	path := filepath.Join(s.blobsDir(), name)
	if _, err := os.Stat(path); err == nil {
		return name, true
	}
	if os.MkdirAll(s.blobsDir(), 0o700) != nil || fsutil.WriteAtomic(path, data, 0o600) != nil {
		return "", false
	}
	return name, true
}

// loadMedia is saveMedia undone: each ref becomes its bytes again. A part
// whose blob is gone becomes a text part saying so — the entry still reads,
// and the model is told media was there.
func (s *sessionStore) loadMedia(parts []llm.ContentPart) []llm.ContentPart {
	for i, p := range parts {
		switch {
		case p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, imageMarker):
			name := filepath.Base(strings.TrimPrefix(p.ImageURL.URL, imageMarker))
			data, err := os.ReadFile(filepath.Join(s.blobsDir(), name))
			if err != nil {
				parts[i] = missingMedia("image", name)
				continue
			}
			parts[i] = llm.ImageData("image/"+strings.TrimPrefix(filepath.Ext(name), "."), data)
		case p.VideoURL != nil && strings.HasPrefix(p.VideoURL.URL, mediaMarker):
			mime, data, name, ok := s.readMediaRef(p.VideoURL.URL)
			if !ok {
				parts[i] = missingMedia("video", name)
				continue
			}
			parts[i] = llm.VideoData(mime, data)
		case p.File != nil && strings.HasPrefix(p.File.FileData, mediaMarker):
			mime, data, name, ok := s.readMediaRef(p.File.FileData)
			if !ok {
				parts[i] = missingMedia("file", name)
				continue
			}
			parts[i] = llm.FileData(mime, p.File.Filename, data)
		case p.InputAudio != nil && strings.HasPrefix(p.InputAudio.Data, mediaMarker):
			_, data, name, ok := s.readMediaRef(p.InputAudio.Data)
			if !ok {
				parts[i] = missingMedia("audio", name)
				continue
			}
			parts[i] = llm.AudioData(p.InputAudio.Format, data)
		}
	}
	return parts
}

func missingMedia(kind, name string) llm.ContentPart {
	return llm.TextPart("[dun: " + kind + " missing from the session — " + name + "]")
}

// readMediaRef reads a "\x00dun-media:<mime> <name>" ref's blob.
func (s *sessionStore) readMediaRef(ref string) (mime string, data []byte, name string, ok bool) {
	mime, name, found := strings.Cut(strings.TrimPrefix(ref, mediaMarker), " ")
	name = filepath.Base(name)
	if !found || !mediaTypeRe.MatchString(mime) {
		return "", nil, name, false
	}
	data, err := os.ReadFile(filepath.Join(s.blobsDir(), name))
	if err != nil {
		return "", nil, name, false
	}
	return mime, data, name, true
}

// mediaTypeRe is a MIME type a ref may name: type/subtype, no parameters.
var mediaTypeRe = regexp.MustCompile(`^[a-z]+/[a-z0-9][a-z0-9.+-]*$`)

// safeMediaSub is a subtype as a file extension: lowercase, [a-z0-9+-]
// only, "bin" when nothing is left.
func safeMediaSub(sub string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(sub) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "bin"
	}
	return b.String()
}

// decodeDataURI splits a base64 data: URI into its MIME type (lowercased,
// a plain type/subtype) and bytes; ok=false for anything else (an https URL
// stays as it is).
func decodeDataURI(u string) (mime string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(u, "data:")
	if !found {
		return "", nil, false
	}
	meta, b64, found := strings.Cut(rest, ",")
	if !found {
		return "", nil, false
	}
	mime, found = strings.CutSuffix(meta, ";base64")
	mime = strings.ToLower(mime)
	if !found || !mediaTypeRe.MatchString(mime) {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, false
	}
	return mime, data, true
}
