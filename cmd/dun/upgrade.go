package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

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
	state    string // pending | restarting | failed
	version  string
	exe      string
	deadline time.Time
	waiting  []proto.UpgradeWait
	err      string // failed: why (nothing was replaced)
}

// parseUpgrade reads an upgrade event.
func parseUpgrade(ev evMsg) *upgradeState {
	u := &upgradeState{state: str(ev["state"]), version: str(ev["version"]), exe: str(ev["exe"]), err: str(ev["error"])}
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

// upgradeFailed is the conversation's note for a failed upgrade: a service's
// dund could not put the new build in place, and runs on.
func upgradeFailed(u *upgradeState) string {
	return "dund could not upgrade to " + buildName(u.version) + " and runs on its own build: " + u.err
}

func buildName(v string) string {
	if v == "" {
		return "a new build"
	}
	return v
}

// upgradeRow is the pending upgrade's status row, at most width runes. It is
// one row always (a list that grew and shrank as work ended made the screen
// bounce), and the way to the detail comes before the detail: a cut at 49
// columns lost "waiting for …" and left no way to learn why (USER
// 2026-10-24). /upgrade shows all of it (upgradeDetail).
func upgradeRow(u *upgradeState, session string, width int) string {
	if u == nil {
		return ""
	}
	if u.state == "restarting" {
		return fit("⟳ dund is restarting into "+buildName(u.version)+"… this reconnects", width)
	}
	head := "⟳ dund upgrade to " + buildName(u.version)
	switch len(u.waiting) {
	case 0:
		return fit(head+" — restarting", width)
	case 1:
		return fit(head+" · /upgrade · waits for "+waitLine(u.waiting[0], session), width)
	}
	return fit(fmt.Sprintf("%s · /upgrade · waits for %d things: %s, …", head, len(u.waiting),
		waitLine(u.waiting[0], session)), width)
}

// upgradeDetail is /upgrade's answer: what dund waits for, each with how to
// end it, when it restarts anyway, and how to restart now. Plain lines; the
// conversation wraps them.
func upgradeDetail(u *upgradeState, session string) string {
	if u.state == "restarting" {
		return "dund is restarting into " + buildName(u.version) + "; this client reconnects when it is back"
	}
	lines := []string{"dund is upgrading to " + buildName(u.version) + "."}
	if len(u.waiting) == 0 {
		return lines[0] + " Nothing is running; it is restarting now."
	}
	when := "It restarts when nothing below is running"
	if !u.deadline.IsZero() {
		when += ", and at " + u.deadline.Format("15:04") + " at the latest"
	}
	lines = append(lines, when+". Waiting for:")
	for _, w := range u.waiting {
		lines = append(lines, "  · "+waitLine(w, session))
	}
	lines = append(lines, "/upgrade now restarts it now; the work it cuts resumes after.")
	return strings.Join(lines, "\n")
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

// upgradeView is the pending upgrade's status row, styled; "" when none.
func (m tuiModel) upgradeView() string {
	if r := upgradeRow(m.upgrade, m.sessionID, m.w); r != "" {
		return stNote.Render(r)
	}
	return ""
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

// attachWait is what an attach in flight waits on, for the status line
// while there is no session yet (startingWhat): dund itself, or the session
// it is creating — and, when dund is another build, which one and the
// command that replaces it. An older dund's engine starts every built-in
// server before it names its session (item 39: ~55 s on poly-lsp, live);
// the TUI cannot see that wait, so it says what it can.
type attachWait struct {
	session   bool   // dund answered; the create is in flight
	dundBuild string // dund's build when it is not this one ("" when it is)
	fix       string // the command that brings dund to this build
}

// attachNow is the attach in flight, or nil. One attach runs at a time
// (restartEngine); the model reads it on the spinner's tick.
var attachNow atomic.Pointer[attachWait]

// noteDundBuild records dund's build from its hello reply, before the
// create: a dund too old to say hello is "an older build" with no number.
func noteDundBuild(ev proto.Event, err error) {
	w := &attachWait{session: true}
	switch r, ok := ev.Payload.(*proto.HelloReply); {
	case err != nil:
		w.dundBuild, w.fix = "?", killLine()+", then dun again"
	case !ok || r.Version == version || r.Version == "dev" || version == "dev":
	case !r.Upgradable:
		exe, _ := selfExe()
		w.dundBuild, w.fix = r.Version, serviceFix(r, exe)
	case !r.Coordinated:
		w.dundBuild, w.fix = r.Version, killLine()+", then dun again"
	}
	attachNow.Store(w)
}

// serviceFix is how to bring a service's dund that cannot upgrade itself (r,
// its hello) to the build at exe, this binary. A restart runs the unit's
// binary again, so it helps only when that is this file. A dund too old to
// upgrade itself as a service still restarts into its own file when a new
// build is renamed over it. One that cannot write there (NoUpgrade) needs the
// service installed from this binary.
func serviceFix(r *proto.HelloReply, exe string) string {
	switch {
	case r.Exe == "" || exe == "" || r.Exe == exe:
		return "dun dund service restart"
	case r.NoUpgrade != "":
		return "install the service from this build: " + exe + " dund service install --start -- <its serve flags>"
	}
	return "put this build where the service runs it, and it restarts into it once nothing is busy: install -m 755 " +
		exe + " " + r.Exe + ".new && mv -f " + r.Exe + ".new " + r.Exe
}

// checkDundBuild compares dund's build with this one, after attaching. It
// returns notes for the conversation, and the build to re-exec this TUI into
// ("" for none). A dund that coordinates upgrades is asked once per build
// pair to move to this binary and then followed by its broadcasts; an older
// dund gets the command that replaces it, once.
func checkDundBuild(ac *attachConn, sc *bufio.Scanner) (notes []string, reexec string) {
	ev, err := ac.call(sc, "", &proto.Hello{Version: version})
	noteDundBuild(ev, err)
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
			why := ""
			if r.NoUpgrade != "" {
				why = ", and cannot upgrade itself: " + r.NoUpgrade
			}
			return []string{"dund (a service) runs build " + r.Version + ", this is " + version + why + " — " + serviceFix(r, exe)}, ""
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
	// Sent by dund's scheduler, not typed now (internal/dund scheduledText):
	// the bracket says when it was set, so the mark is all the row adds.
	if strings.HasPrefix(text, "[scheduled ") || strings.HasPrefix(text, "[loop ") {
		row = "⏲ " + text
	}
	if width > 0 {
		st = st.Width(width)
	}
	return st.Render(row)
}

// upgradeIntoSelfCmd asks dund to upgrade into this TUI's own binary and to
// restart now: a source build's /upgrade now, which nothing else triggers
// (the launcher's rebuild reloads TUIs, not dund).
func upgradeIntoSelfCmd(p *dunProc) tea.Cmd {
	return func() tea.Msg {
		exe, err := selfExe()
		if err != nil {
			return upgradeSelfMsg{err: err}
		}
		if !askDundUpgrade(exe, version) {
			return upgradeSelfMsg{err: errors.New("dund did not take it: it is not running here, or a service manager owns it (dun dund status says)")}
		}
		if !p.upgradeNow() {
			return upgradeSelfMsg{err: errors.New("dund took the upgrade but the restart could not be asked for: it starts when nothing is busy")}
		}
		return upgradeSelfMsg{}
	}
}

// upgradeSelfMsg is upgradeIntoSelfCmd's answer.
type upgradeSelfMsg struct{ err error }
