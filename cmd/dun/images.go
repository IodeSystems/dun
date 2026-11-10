package main

import (
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/proto"
)

// images.go — the engine's half of images on a user message: the vision
// config, and the refusal a client turns back into a draft. The TUI's half
// (clipboard, pasted paths, tokens) is in imageinput.go; the rest is in the
// dun package (images.go there).

// visionConfig is what the engine running model on url needs for images.
// The describer is the vision role as dunconfig resolves it (facets.vision,
// else vision_model); a role that is off is as if none were set.
func visionConfig(vr dunconfig.Resolved, url, model string, usageFor func(url, key, model string) func(llm.Usage)) dun.VisionConfig {
	v := dun.VisionConfig{Key: dun.VisionKey(url, model), Model: model}
	if !vr.On() {
		return v
	}
	c := llm.NewClient(vr.URL, vr.Key, vr.Model)
	c.OnUsage = usageFor(vr.URL, vr.Key, vr.Model)
	v.Describer, v.DescriberModel = c, vr.Model
	return v
}

// dunImages is the wire's images as the harness takes them.
func dunImages(imgs []proto.Image) []dun.ImageInput {
	out := make([]dun.ImageInput, len(imgs))
	for i, im := range imgs {
		out[i] = dun.ImageInput{Path: im.Path, Label: im.Label}
	}
	return out
}

// refuseImages hands a message with images back to the user: why it was
// not sent, and the message itself so the client puts it back in the
// composer. done closes the turn the client opened when it sent.
func refuseImages(em *emitter, why, content string, imgs []proto.Image) {
	em.emit(&proto.ImageRefused{Message: why, Content: content, Images: imgs})
	em.emit(&proto.Done{})
}
