package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun/internal/buildinfo"
)

var (
	oldJobsCmds = map[string]any{"type": "commands", "items": []any{
		map[string]any{"name": "jobs", "usage": "[kill <n>|kill all]", "description": "background jobs",
			"choices": []any{map[string]any{"value": "kill"}}},
	}}
	newJobsCmds = map[string]any{"type": "commands", "items": []any{
		map[string]any{"name": "jobs", "usage": "[kill <n>|kill all|clear]", "description": "background jobs",
			"choices": []any{map[string]any{"value": "kill"}, map[string]any{"value": "clear"}}},
	}}
)

// Carl's /version: an engine that reports no build is not "(same)".
func TestVersionsReport_UnknownIsNotSame(t *testing.T) {
	plain := func(ls []string) string {
		for i := range ls {
			ls[i] = ansi.Strip(ls[i])
		}
		return strings.Join(ls, "\n")
	}
	if got := plain(versionsReport("v0.9.1", "v0.9.1", "")); strings.Contains(got, "(same)") {
		t.Errorf("engine not yet known:\n%s", got)
	}
	got := plain(versionsReport("v0.9.1", "v0.9.1", engineUnreported))
	if strings.Contains(got, "(same)") || !strings.Contains(got, "engine not reported (built before 0681425, so older than this TUI)") ||
		!strings.Contains(got, "but engine is not") {
		t.Errorf("engine that reports no build:\n%s", got)
	}
	if got := plain(versionsReport("v0.9.1", "v0.9.1", "v0.9.1")); !strings.HasSuffix(got, "(same)") {
		t.Errorf("all the same:\n%s", got)
	}
}

// A TUI attaching to a running engine through dund gets its ready and
// commands in the snapshot's state; when the engine moves to a new build,
// the new ready and commands replace them: the build and /jobs clear.
func TestTUI_EngineBuildFollowsTheEngine(t *testing.T) {
	v := newVtui(100, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	if got := v.m.engineBuild(); got != "" {
		t.Fatalf("before a ready: %q", got)
	}
	// Attached late: the old engine's ready (no version) and commands.
	v.event(map[string]any{"type": "snapshot", "after": 9.0, "items": []any{}, "state": []any{
		map[string]any{"type": "ready", "tools": []any{"exec"}, "replayed": true},
		func() map[string]any { c := map[string]any{"replayed": true}; copyInto(c, oldJobsCmds); return c }(),
	}})
	if got := v.m.engineBuild(); got != engineUnreported {
		t.Errorf("an engine that says no build: %q", got)
	}
	if !v.m.engineLacks("jobs", []string{"clear"}) {
		t.Fatal("old commands: clear is not known to be missing")
	}
	if l := v.m.engineOldLine("jobs", []string{"clear"}); strings.Contains(l, "(an older build)") ||
		!strings.Contains(l, "has no /jobs clear, so it is older than this TUI") || !strings.Contains(l, "does not report its build") {
		t.Errorf("unknown build: %q", l)
	}
	// dund named it from the link hello.
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}, "version": "v0.9.1-20-gaaaaaaa"})
	if l := v.m.engineOldLine("jobs", []string{"clear"}); !strings.Contains(l, "engine is older (v0.9.1-20-gaaaaaaa)") {
		t.Errorf("known build: %q", l)
	}
	// The move: stopping, started, the new ready and commands.
	v.event(map[string]any{"type": "engine", "state": "stopping", "reason": "moving from build v0.9.1-20-gaaaaaaa to " + buildinfo.Version})
	v.event(map[string]any{"type": "engine", "state": "started"})
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}, "version": buildinfo.Version})
	v.event(newJobsCmds)
	if v.m.engineVersion != buildinfo.Version || v.m.engineLacks("jobs", []string{"clear"}) {
		t.Errorf("after the move: version %q, lacks clear %v", v.m.engineVersion, v.m.engineLacks("jobs", []string{"clear"}))
	}
}

func copyInto(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// /upgrade now when dund is already this build but this session's engine
// is not (adopted, held by a job): it asks dund to move the engine, and
// does not restart dund again (which would adopt it again).
func TestUpgradeNow_MovesAnOldEngine(t *testing.T) {
	old, oldAsk := runTools, askDundUpgrade
	defer func() { runTools, askDundUpgrade = old, oldAsk }()
	runTools = func(bool) ([]toolState, error) { return nil, nil }
	restarted := false
	askDundUpgrade = func(string, string) bool { restarted = true; return true }
	oldV := buildinfo.Version
	buildinfo.Version = "v0.9.1-33-g6996185"
	defer func() { buildinfo.Version = oldV }()
	dundSeen.Store("v0.9.1-33-g6996185")
	defer dundSeen.Store("")

	run := func(engine string, args ...string) (sent, said string) {
		rec := &recWC{}
		m := newTUIModel(&dunProc{stdin: rec}, "/ws")
		m.engineVersion, m.engineReady = engine, true
		cmd := upgradeSlash(&m, args)
		if follow := m.toolsDone(cmd().(toolsMsg)); follow != nil {
			follow()
		}
		return rec.String(), ansi.Strip(m.convo[len(m.convo)-1].view())
	}
	for _, engine := range []string{"", "v0.9.1-20-gaaaaaaa"} {
		sent, said := run(engine, "now")
		if !strings.Contains(sent, `"type":"move_engine"`) || strings.Contains(sent, `"force":true`) || restarted {
			t.Errorf("engine %q: sent %q, dund restarted %v", engine, sent, restarted)
		}
		if !strings.Contains(said, "into it if nothing holds it (dund says what does)") {
			t.Errorf("engine %q: said %q", engine, said)
		}
		sent, said = run(engine, "now", "--force")
		if !strings.Contains(sent, `"type":"move_engine"`) || !strings.Contains(sent, `"force":true`) || restarted {
			t.Errorf("engine %q --force: sent %q, dund restarted %v", engine, sent, restarted)
		}
		if !strings.Contains(said, "into it now — its running turn, jobs and sub-agents end") {
			t.Errorf("engine %q --force: said %q", engine, said)
		}
	}

	// An engine on this build: dund is restarted as before (a source build).
	if sent, _ := run(buildinfo.Version, "now"); strings.Contains(sent, "move_engine") {
		t.Errorf("moved a current engine: %q", sent)
	}
}

// dund's answer to a held engine's move (an ack error, which the attach
// turns into a notification) is shown where the person looks.
func TestUpgradeNow_BusyAnswerShown(t *testing.T) {
	v := newVtui(100, 24)
	v.event(map[string]any{"type": "notification", "text": "dund: this session's engine is busy (1 job running, agent #2 working): it moves when they finish; /upgrade now --force ends them and moves now"})
	if got := v.convo(); !strings.Contains(got, "busy (1 job running, agent #2 working)") {
		t.Errorf("not shown:\n%s", got)
	}
}
