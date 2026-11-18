// Package psview is `dun ps --watch` on a terminal: the process table as
// an interactive view (issue i_1m7pgr5bc4bbfc89kw41m3, USER 2026-11-17).
// Rows are grouped by session and fold on enter; S cycles the sort; each
// row carries its CPU over the last minute; the selected row can be shown
// in full, killed, its session stopped or opened, and the poly-lsp daemon
// restarted. Every confirm is a y/n line in the footer.
//
// The data is internal/procps's, read by internal/dundcli (dundcli.PSSource);
// the styles are internal/tui's. dundcli cannot import this package (tui
// imports dundcli), so cmd/dun sets dundcli.Interactive to Run.
package psview

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
)

// Actions are what the view does to the machine; tests replace them.
type Actions struct {
	// Kill sends SIGTERM to pid, or SIGKILL when force.
	Kill func(pid int, force bool) error
	// Stop stops a session's engine through dund.
	Stop func(id string) error
	// Restart runs `poly-lsp-mcp daemon --restart` for the daemon whose
	// argv is given.
	Restart func(argv []string) error
}

// Run shows the view until q, then execs `dun --resume <id>` when o asked
// for a session.
func Run(ctx context.Context, src dundcli.PSSource) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m := newModel(src.Sample, Actions{Kill: kill, Stop: src.StopSession, Restart: restartDaemon}, src.Every, procps.Cwd)
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, tea.ErrInterrupted) {
			return nil
		}
		return err
	}
	if fm, ok := final.(*model); ok && fm.open != nil {
		return execResume(fm.open.id, fm.open.workspace)
	}
	return nil
}

// restartTimeout bounds `poly-lsp-mcp daemon --restart`.
const restartTimeout = 30 * time.Second

// restartDaemon runs the daemon's own binary with --restart, keeping its
// --daemon-home (as procps.ReadRoots does for status).
func restartDaemon(argv []string) error {
	bin := "poly-lsp-mcp"
	if len(argv) > 0 && filepath.IsAbs(argv[0]) {
		bin = argv[0]
	}
	args := []string{"daemon", "--restart"}
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, "--daemon-home="); ok {
			args = append(args, "--daemon-home="+v)
		} else if a == "--daemon-home" && i+1 < len(argv) {
			args = append(args, "--daemon-home="+argv[i+1])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("no answer in %s", restartTimeout)
	}
	if err != nil {
		if m := strings.Join(strings.Fields(string(out)), " "); m != "" {
			return errors.New(m)
		}
		return err
	}
	return nil
}
