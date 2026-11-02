package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

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
