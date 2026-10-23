package main

import (
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// images.go — the engine's half of images on a user message: the vision
// config, and the refusal a client turns back into a draft. The TUI's half
// (clipboard, pasted paths, tokens) is in imageinput.go; the rest is in the
// dun package (images.go there).

// visionConfig is what the engine running model on url needs for images.
// The vision model is on the provider vision_provider names, else on url.
func visionConfig(fc dunConfig, url, key, model string, onUsage func(llm.Usage)) dun.VisionConfig {
	v := dun.VisionConfig{Key: dun.VisionKey(url, model), Model: model}
	if fc.VisionModel == "" {
		return v
	}
	vu, vk := url, key
	if fc.VisionProvider != "" {
		p, ok := fc.named(fc.VisionProvider)
		if !ok {
			return v // a provider not logged in: as if none were set
		}
		vu, vk = p.URL, p.Key
	}
	c := llm.NewClient(vu, vk, fc.VisionModel)
	c.OnUsage = onUsage
	v.Describer, v.DescriberModel = c, fc.VisionModel
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
