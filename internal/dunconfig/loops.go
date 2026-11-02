package dunconfig

import (
	"github.com/iodesystems/agentkit/loopdetector"
)

// LoopConfig is config.json's "loops". Zero means agentkit's default.
type LoopConfig struct {
	Mode          string  `json:"mode,omitempty"`           // off, count or cut; "" is count
	Tolerance     int     `json:"tolerance,omitempty"`      // the loop count that cuts
	ToolTolerance int     `json:"tool_tolerance,omitempty"` // the same for tool calls; negative counts them only
	Window        int     `json:"window,omitempty"`         // recent sentences compared
	MinShare      float64 `json:"min_share,omitempty"`      // the share of them that must repeat
	Similarity    float64 `json:"similarity,omitempty"`     // BM25 share that makes two sentences one; negative: exact only
	ToolWindow    int     `json:"tool_window,omitempty"`    // recent tool calls compared
}

const (
	LoopsOff = "off"
)

const (
	LoopsCount = "count"
)

// EffectiveMode is cfg's mode with the default filled in.
func (cfg LoopConfig) EffectiveMode() string {
	if cfg.Mode == "" {
		return LoopsCount
	}
	return cfg.Mode
}

func (cfg LoopConfig) Detector() *loopdetector.LoopDetector {
	c := loopdetector.Config{
		Off:           cfg.EffectiveMode() == LoopsOff,
		Tolerance:     cfg.Tolerance,
		ToolTolerance: cfg.ToolTolerance,
		Window:        cfg.Window,
		MinShare:      cfg.MinShare,
		Similarity:    cfg.Similarity,
		ToolWindow:    cfg.ToolWindow,
	}
	if cfg.EffectiveMode() == LoopsCount {
		c.Tolerance, c.ToolTolerance = -1, -1
	}
	return loopdetector.New(c)
}
