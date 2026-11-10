package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/depver"
	"github.com/iodesystems/dun/internal/textfmt"
)

// /upgrade covers the tools too (USER 2026-11-04): bare, it says dund's
// pending upgrade and each tool against the version this dun pins; now, it
// installs the tools that are behind (`dun tools --upgrade`: the pinned
// download, or go install), then restarts dund, so the engines start on the
// new tools. The version is always the pin (release/deps.json, raised by the
// signed registry), never a tool's latest release.

// toolState is one row of `dun tools --json` (cmd/dun/toolscmd.go).
type toolState struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Origin  string `json:"origin"`
	Target  string `json:"target"`
	Note    string `json:"note"`
	Fix     string `json:"fix"`
	Can     bool   `json:"can"`
	Updated bool   `json:"updated"`
	Error   string `json:"error"`
	Missing bool   `json:"missing"`
	Pin     string `json:"pin"`
	Release bool   `json:"release"`
}

// dep is the row as /version shows it.
func (r toolState) dep() depver.Tool {
	return depver.Tool{Name: r.Name, Path: r.Path, Version: r.Version, Origin: r.Origin,
		Pin: r.Pin, Release: r.Release, Missing: r.Missing}
}

// toolsMsg is `dun tools`'s answer; upgrade: it was --upgrade (/upgrade now).
type toolsMsg struct {
	rows    []toolState
	upgrade bool
	err     error
}

// runTools is a variable for the tests.
var runTools = func(upgrade bool) ([]toolState, error) {
	exe, err := buildinfo.SelfExe()
	if err != nil {
		return nil, err
	}
	args := []string{"tools", "--json"}
	if upgrade {
		args = append(args, "--upgrade")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).Output()
	var rows []toolState
	if jerr := json.Unmarshal(out, &rows); jerr != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, errors.New(textfmt.OneLine(string(ee.Stderr)))
		}
		if err != nil {
			return nil, err
		}
		return nil, jerr
	}
	return rows, nil // a failed update is in its row
}

func toolsCmd(upgrade bool) tea.Cmd {
	return func() tea.Msg {
		rows, err := runTools(upgrade)
		return toolsMsg{rows: rows, upgrade: upgrade, err: err}
	}
}

// upgradeSlash is /upgrade.
func upgradeSlash(m *tuiModel, args []string) tea.Cmd {
	if ch, ok := channelArg(args); ok {
		return channelSlash(m, ch)
	}
	if len(args) > 0 && (args[0] == "off" || args[0] == "on") {
		line, err := upgradeSwitch(args[0] == "on")
		if err != nil {
			m.append(StErr.Render("updates: not saved: " + err.Error()))
			return nil
		}
		m.append(StDim.Render(line))
		return nil
	}
	if len(args) > 0 && args[0] == "now" {
		m.append(StDim.Render("tools: installing what is behind its pinned version, then restarting dund…"))
		return toolsCmd(true)
	}
	if m.upgrade != nil {
		m.append(stNote.Render(upgradeDetail(m.upgrade, m.sessionID)))
	} else {
		line := "no dund upgrade is pending"
		if !buildinfo.IsRelease() {
			// A source build: nothing tells dund there is a newer build, so
			// dund and its engines keep the one they started with (USER
			// 2026-10-27: /upgrade on a source build was a dead end).
			line += " · dund and its engines keep the build they started with; /upgrade now restarts them into this one (" + buildinfo.Version + ")"
		}
		m.append(StDim.Render(line))
	}
	return toolsCmd(false)
}

// toolsDone shows the tools' rows; after /upgrade now it restarts dund.
func (m *tuiModel) toolsDone(msg toolsMsg) tea.Cmd {
	if msg.err != nil {
		m.append(StErr.Render("tools: not checked: " + msg.err.Error()))
	} else {
		m.append(toolsBlock(msg.rows, msg.upgrade))
	}
	if !msg.upgrade {
		return nil
	}
	updated := false
	for _, r := range msg.rows {
		updated = updated || r.Updated
	}
	switch {
	case m.upgrade != nil:
		if !m.proc.upgradeNow() {
			m.append(StErr.Render("no session service right now — nothing restarted"))
			return nil
		}
		m.append(StDim.Render("asked dund to restart now — running turns are interrupted and resume after"))
		return nil
	case updated || !buildinfo.IsRelease():
		// The engines run the tools: a restart is how they take the new
		// ones. A source build also moves dund to this TUI's build.
		m.append(StDim.Render("asking dund to restart into this build (" + buildinfo.Version + ") — running turns are interrupted and resume after"))
		return upgradeIntoSelfCmd(m.proc)
	}
	m.append(StDim.Render("nothing to upgrade: dund and the tools are current"))
	return nil
}

// toolsBlock is the tools' rows as /upgrade shows them.
func toolsBlock(rows []toolState, upgraded bool) string {
	lines := []string{stHeader.Render("tools") + StDim.Render(" — against the versions this dun pins")}
	for _, r := range rows {
		lines = append(lines, "  "+toolLine(r, upgraded))
	}
	return strings.Join(lines, "\n")
}

func toolLine(r toolState, upgraded bool) string {
	name := r.Name + ": "
	switch {
	case r.Missing:
		s := "not installed"
		if r.Fix != "" {
			s += " (" + r.Fix + ")"
		}
		return name + StDim.Render(s)
	case r.Error != "":
		return name + StErr.Render("the update to "+r.Target+" failed: "+textfmt.Clip(r.Error, 120))
	case r.Updated:
		return name + stNote.Render("updated to "+r.Version)
	}
	s := r.Origin
	if r.Note != "" {
		s += "; " + r.Note
	}
	switch {
	case r.Target == "" || r.Fix == "":
		return name + StDim.Render(s)
	case r.Can && !upgraded:
		return name + s + StDim.Render(" — /upgrade now installs it")
	}
	return name + s + StDim.Render(" — "+r.Fix)
}
