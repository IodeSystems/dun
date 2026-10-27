package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iodesystems/dun"
)

// mediaFlag is --media <file>, repeatable: files to attach to a one-shot
// task's first message.
type mediaFlag []string

func (m *mediaFlag) String() string { return fmt.Sprint([]string(*m)) }

func (m *mediaFlag) Set(s string) error {
	if s == "" {
		return fmt.Errorf("--media needs a file")
	}
	*m = append(*m, s)
	return nil
}

// resolve makes every path absolute and checks it is an image the model can
// take (png, jpeg, gif, webp), before anything starts.
func (m mediaFlag) resolve() error {
	for i, p := range m {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("--media %s: %v", p, err)
		}
		if err := dun.CheckImage(abs); err != nil {
			return fmt.Errorf("--media %s: %v", p, err)
		}
		m[i] = abs
	}
	return nil
}

// inputs are the files as the harness takes them, labelled like the TUI's.
func (m mediaFlag) inputs() []dun.ImageInput {
	var out []dun.ImageInput
	for _, p := range m {
		out = append(out, dun.ImageInput{Path: p})
	}
	return out
}
