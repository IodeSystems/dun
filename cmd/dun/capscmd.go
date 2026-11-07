package main

import (
	"context"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

// capsClient is the session's main client, for /model and /model probe. The
// record shown is this client's: its endpoint and model (capstore.go).
var capsClient *llm.Client

// capsControl answers the engine's "model" control: bare shows what is known
// about the session's model and how each fact was learned; probe checks now,
// with a few short requests, and shows the result.
func capsControl(ctx context.Context, action string) string {
	if capsClient == nil {
		return "model: no client"
	}
	switch action {
	case "", "caps":
		return dun.CapsReport(capsClient.Capabilities())
	case "probe":
		caps, err := llm.ProbeCapabilities(ctx, capsClient, llm.ProbeOpts{})
		msg := dun.CapsReport(caps)
		if err != nil {
			msg += "\nprobe stopped: " + err.Error()
		}
		return msg
	}
	return "usage: /model caps | /model probe"
}
