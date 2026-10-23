package dun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// The /repaint report model call (cmd/dun/repaintreport.go builds it).
//
// The TUI writes a capture to $DUN_HOME/bugreports/<ts>/, runs the
// deterministic redaction itself, and puts the exact prompt in prompt.txt.
// This side only sends it: one round trip on the session's model, no tools,
// no history, the answer written to model-output.txt for the TUI to split up.
// Files rather than the event stream, because every client attached to the
// session sees an event, and the capture is not theirs to see.
//
// Measured as a side call (kind "repaint"), as every helper call is.

// RepaintReportDir checks that dir is a capture directory: under
// $DUN_HOME/bugreports, one level down. The engine reads and writes there on a
// client's word, so the word has to name a capture and nothing else.
func RepaintReportDir(dir string) (string, error) {
	root := filepath.Join(DunHome(), "bugreports")
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) != root {
		return "", fmt.Errorf("not a capture directory under %s: %s", root, dir)
	}
	return clean, nil
}

// RepaintReport sends dir/prompt.txt to the model and writes the answer to
// dir/model-output.txt.
func (h *Harness) RepaintReport(ctx context.Context, dir string) error {
	if h.Session == nil || h.Session.Runner == nil {
		return fmt.Errorf("no model to ask")
	}
	dir, err := RepaintReportDir(dir)
	if err != nil {
		return err
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		return err
	}
	start := time.Now()
	ch, err := untee(h.Session.Runner).ChatStream(ctx, []llm.Message{{Role: "user", Content: string(prompt)}}, nil, nil)
	if err != nil {
		h.noteSideCall("repaint", start, nil)
		return err
	}
	var out strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("repaint", start, nil)
			return fmt.Errorf("%s", c.Error)
		}
		out.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("repaint", start, usage)
	if strings.TrimSpace(out.String()) == "" {
		return fmt.Errorf("the model returned nothing")
	}
	return os.WriteFile(filepath.Join(dir, "model-output.txt"), []byte(out.String()), 0o600)
}
