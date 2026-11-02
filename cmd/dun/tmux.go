package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ── dun tmux ───────────────────────────────────────────────────────

// tmuxIconPrefix goes in front of a window-status format: the icon and a
// space when the window has one, nothing otherwise.
const tmuxIconPrefix = "#{?@dun_icon,#{@dun_icon} ,}"

// tmuxDefaultFormat is tmux's own window-status-format default.
const tmuxDefaultFormat = "#I:#W#{?window_flags,#{window_flags}, }"

// tmuxQuery returns a tmux command's output.
type tmuxQuery func(args ...string) (string, error)

func execTmuxQuery(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	return strings.TrimRight(string(out), "\n"), err
}

// tmuxCmd is `dun tmux`: the tmux.conf lines that show the icon, and whether
// the running tmux shows it already. inTmux is $TMUX being set; iterm is
// LC_TERMINAL=iTerm2, for the passthrough line the tab escapes need.
func tmuxCmd(w io.Writer, inTmux, iterm bool, q tmuxQuery) int {
	fmt.Fprintln(w, "dun marks its tmux window with @dun_icon (⚙ working, ❓ question, ✓ done) and @dun_state; add these lines to ~/.tmux.conf to show the icon in the window list:")
	fmt.Fprintln(w)
	shown := 0
	for _, opt := range []string{"window-status-format", "window-status-current-format"} {
		cur, err := "", error(nil)
		if inTmux {
			cur, err = q("show-options", "-gv", opt)
		}
		if strings.Contains(cur, "@dun_icon") {
			shown++
		}
		// Built on the current format, so the user's own styling stays; a
		// format with a single quote cannot be quoted in one, so the default.
		base := cur
		if !inTmux || err != nil || cur == "" || strings.Contains(cur, "'") || strings.Contains(cur, "@dun_icon") {
			base = tmuxDefaultFormat
		}
		fmt.Fprintf(w, "  set -g %s '%s%s'\n", opt, tmuxIconPrefix, base)
	}
	if iterm {
		fmt.Fprintln(w, "  set -g allow-passthrough on   # iTerm2: lets dun colour and title the tab from inside tmux")
	}
	fmt.Fprintln(w)
	switch {
	case !inTmux:
		fmt.Fprintln(w, "not inside tmux: run dun tmux in tmux to check what it shows")
	case shown == 2:
		fmt.Fprintln(w, "this tmux shows the icon already")
	case shown == 1:
		fmt.Fprintln(w, "this tmux shows the icon in one of the two formats only")
	default:
		fmt.Fprintln(w, "this tmux does not show the icon yet; after editing, run: tmux source-file ~/.tmux.conf")
	}
	fmt.Fprintln(w, "a question also rings the terminal bell, which tmux marks in the window list; DUN_BELL=off turns it off")
	return 0
}
