package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
)

// A coordinated upgrade, as the TUI sees it. dund broadcasts a pending
// upgrade (proto.UpgradeStatus) to every connection; the TUI shows it on a
// status row of its own — what dund waits for, and how to end each — until
// dund restarts. After the reconnect, a TUI whose own binary on disk is now
// dund's build re-execs into it, on the same session (/reload's mechanism).

// upgradeState is the last upgrade status dund sent; nil: none pending.
type upgradeState struct {
	state    string // pending | restarting
	version  string
	exe      string
	deadline time.Time
	waiting  []proto.UpgradeWait
}

// parseUpgrade reads an upgrade event.
func parseUpgrade(ev evMsg) *upgradeState {
	u := &upgradeState{state: str(ev["state"]), version: str(ev["version"]), exe: str(ev["exe"])}
	if d, ok := ev["deadline"].(float64); ok && d > 0 {
		u.deadline = time.Unix(int64(d), 0)
	}
	ws, _ := ev["waiting"].([]any)
	for _, w := range ws {
		wm, _ := w.(map[string]any)
		id, _ := wm["id"].(float64)
		u.waiting = append(u.waiting, proto.UpgradeWait{Session: str(wm["session"]), What: str(wm["what"]),
			ID: int(id), Detail: str(wm["detail"]), Hint: str(wm["hint"])})
	}
	return u
}

func buildName(v string) string {
	if v == "" {
		return "a new build"
	}
	return v
}

// upgradeRows is the pending upgrade as rows, each at most width runes:
// the headline, then one row per thing dund waits for. session is the one
// this TUI shows, whose work it can end itself (ctrl+c, /jobs kill).
func upgradeRows(u *upgradeState, session string, width int) []string {
	if u == nil {
		return nil
	}
	if u.state == "restarting" {
		return []string{fit("⟳ dund is restarting into "+buildName(u.version)+"… this reconnects", width)}
	}
	head := "⟳ dund is upgrading to " + buildName(u.version)
	if len(u.waiting) == 0 {
		return []string{fit(head+" — restarting", width)}
	}
	// The list goes last: a deadline or a hint after "waiting for:" read as
	// the list, and an empty one (USER 2026-10-23). One wait fits on the row.
	head += " — restarts"
	if !u.deadline.IsZero() {
		head += " at " + u.deadline.Format("15:04") + ","
	}
	head += " or now with /upgrade now (cut work resumes after); waiting for"
	if len(u.waiting) == 1 {
		return []string{fit(head+" "+waitLine(u.waiting[0], session), width)}
	}
	rows := []string{fit(head+":", width)}
	const most = 4
	for i, w := range u.waiting {
		if i == most {
			rows = append(rows, fit(fmt.Sprintf("   · and %d more", len(u.waiting)-most), width))
			break
		}
		rows = append(rows, fit("   · "+waitLine(w, session), width))
	}
	return rows
}

// waitLine is one thing dund waits for, and how to end it from here (ctrl+c interrupts a turn, as ctrl+g does).
func waitLine(w proto.UpgradeWait, session string) string {
	here := w.Session == session
	where := "in session " + w.Session + " (another client)"
	if here {
		where = "in this session"
	}
	switch w.What {
	case "turn":
		if here {
			return "the turn in this session (ctrl+c interrupts)"
		}
		return "a turn " + where
	case "question":
		if here {
			return "a question in this session (answer it, or ctrl+c to interrupt)"
		}
		return "a question " + where
	case "job":
		s := fmt.Sprintf("job #%d %s", w.ID, oneLine(w.Detail))
		if here {
			return s + fmt.Sprintf(" (/jobs kill %d)", w.ID)
		}
		return s + " " + where
	case "agent":
		s := fmt.Sprintf("sub-agent #%d ", w.ID)
		if here {
			return s + "in this session (ctrl+c interrupts its turn)"
		}
		return s + where
	}
	return w.What + " " + where
}

// fit cuts s to width runes (an ellipsis marks the cut).
func fit(s string, width int) string {
	r := []rune(s)
	if width <= 1 || len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

// upgradeView is the pending upgrade's rows, styled; "" when none.
func (m tuiModel) upgradeView() string {
	rows := upgradeRows(m.upgrade, m.sessionID, m.w)
	if len(rows) == 0 {
		return ""
	}
	for i := range rows {
		rows[i] = stNote.Render(rows[i])
	}
	return strings.Join(rows, "\n")
}

// What a TUI does about a dund of another build.
type buildAction int

const (
	buildsAgree  buildAction = iota // same build (or a dev build): nothing
	reexecSelf                      // this binary on disk is dund's build: re-exec into it
	upgradeDund                     // this binary is still this build: ask dund to move to it
	buildsDiffer                    // three builds: say so, once
)

// buildDecision: mine is this process's build, dund dund's, onDisk what this
// process's executable file is now ("" when it would not say).
func buildDecision(mine, dund, onDisk string) buildAction {
	switch {
	case mine == dund || dund == "" || dund == "dev" || mine == "dev":
		return buildsAgree
	case onDisk == dund:
		return reexecSelf
	case onDisk == mine || onDisk == "":
		return upgradeDund
	}
	return buildsDiffer
}

// selfExe is this binary's file, symlinks resolved.
func selfExe() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return exe, err
}

// probeSelf asks this binary's file which build it is now (a variable for
// the tests).
var probeSelf = func(exe string) string { return dundcli.ProbeVersion(exe) }

func killLine() string {
	return "kill $(cat " + filepath.Join(dunHome(), "dund.pid") + ")"
}

// once reports whether key is new (and records it): a note about a build
// pair is said once per process, never on every reconnect.
func once(key string) bool {
	if upgradeAsked[key] {
		return false
	}
	upgradeAsked[key] = true
	return true
}

// checkDundBuild compares dund's build with this one, after attaching. It
// returns notes for the conversation, and the build to re-exec this TUI into
// ("" for none). A dund that coordinates upgrades is asked once per build
// pair to move to this binary and then followed by its broadcasts; an older
// dund gets the command that replaces it, once.
func checkDundBuild(ac *attachConn, sc *bufio.Scanner) (notes []string, reexec string) {
	ev, err := ac.call(sc, "", &proto.Hello{Version: version})
	if err != nil {
		if once("pre-hello") {
			return []string{"dund is an older build that cannot be upgraded from here — stop it (" + killLine() +
				") and start dun again; sessions are kept, a running turn is cut"}, ""
		}
		return nil, ""
	}
	r, ok := ev.Payload.(*proto.HelloReply)
	if !ok || r.Version == version || r.Version == "dev" || version == "dev" {
		return nil, ""
	}
	pair := r.Version + "→" + version
	if !r.Coordinated {
		if once("old " + pair) {
			return []string{"dund runs build " + r.Version + " (this is " + version + "), too old to coordinate an upgrade: stop it (" +
				killLine() + ") and start dun again — sessions are kept; a running turn is cut"}, ""
		}
		return nil, ""
	}
	exe, err := selfExe()
	if err != nil {
		if once("exe " + pair) {
			return []string{"dund runs build " + r.Version + ", this is " + version + "; cannot name this binary to upgrade it: " + err.Error()}, ""
		}
		return nil, ""
	}
	onDisk := probeSelf(exe)
	switch buildDecision(version, r.Version, onDisk) {
	case buildsAgree:
		return nil, ""
	case reexecSelf:
		return []string{"dund runs build " + r.Version + " and " + exe + " is that build now — restarting this TUI into it, same session"}, r.Version
	case buildsDiffer:
		if once("differ " + pair + " " + onDisk) {
			return []string{"dund runs build " + r.Version + ", this TUI is " + version + ", and " + exe + " is now " + onDisk +
				" — run dun again to use one build"}, ""
		}
		return nil, ""
	}
	// This binary is this build and dund is not.
	if !r.Upgradable {
		if once("service " + pair) {
			return []string{"dund (a service) runs build " + r.Version + ", this is " + version + " — dun dund service restart"}, ""
		}
		return nil, ""
	}
	if r.Upgrade != nil && r.Upgrade.Exe == exe {
		return nil, "" // already pending to this binary: the status row shows it
	}
	if upgradeAsked[pair] {
		// Asked before, dund restarted (nothing is pending) and is still
		// the old build: the binary it was handed is not this build.
		if once("stale " + pair) {
			return []string{"dund still runs build " + r.Version + " after restarting into " + exe +
				" — that file is not build " + version + ": stop dund (" + killLine() + ") and run dun from the build you want"}, ""
		}
		return nil, ""
	}
	upgradeAsked[pair] = true
	if _, err := ac.call(sc, "", &proto.Upgrade{Exe: exe, Version: version}); err != nil {
		return []string{"dund runs build " + r.Version + ", this is " + version + "; it would not upgrade: " + err.Error()}, ""
	}
	return nil, "" // the broadcast that follows says what it waits for
}

// upgradeNow asks dund to restart into the pending upgrade at once.
func (p *dunProc) upgradeNow() bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]any{"type": "upgrade", "now": true}) == nil
}

// autoResumePrefix starts the message dund wakes a session with after a
// restart cut its work short (internal/dund resumePrefix).
const autoResumePrefix = "[automatic] "

// userRow is a user message as a conversation row: the person's, or dund's
// automatic resume, shown dim as what it is.
func userRow(text string) string { return userRowAt(text, 0) }

// bandCap is the widest the question band fills: on a wide screen a band to
// the far edge is mostly empty fill.
const bandCap = 100

// userRowAt is userRow wrapped to width (0: unwrapped), its band filled to
// width or bandCap, whichever is less — every wrapped row to the same edge.
func userRowAt(text string, width int) string {
	width = min(width, bandCap)
	st, row := stUser, "› "+text
	if rest, ok := strings.CutPrefix(text, autoResumePrefix); ok {
		st, row = stDim, "↻ resumed after the restart — "+rest
	}
	if width > 0 {
		st = st.Width(width)
	}
	return st.Render(row)
}
