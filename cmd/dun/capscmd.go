package main

import (
	"context"
	"strings"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/ragd"
)

// capsClient is the session's main client, for /model and /model probe. The
// record shown is this client's: its endpoint and model (capstore.go).
var capsClient *llm.Client

// modelFlags are this engine's model flags by role (main.go), so /model
// probe child probes the child as this session runs it.
var modelFlags dunconfig.Overrides

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
	case "probe", "probe " + dunconfig.FacetMain:
		caps, err := llm.ProbeCapabilities(ctx, capsClient, llm.ProbeOpts{})
		msg := dun.CapsReport(caps)
		if err != nil {
			msg += "\nprobe stopped: " + err.Error()
		}
		return msg
	}
	// Another role, as dunconfig resolves it from this engine's flags and
	// the config's facets (docs/rag.md, Model roles).
	if facet, ok := strings.CutPrefix(action, "probe "); ok {
		res, err := ragd.ProbeWith(ctx, dunconfig.LoadConfig(), modelFlags, dun.CapsDir(), facet)
		if err != nil {
			return err.Error()
		}
		return res[0].Report()
	}
	return "usage: /model caps | /model probe [" + strings.Join(dunconfig.FacetNames, "|") + "]"
}
