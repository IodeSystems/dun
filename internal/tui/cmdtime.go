package tui

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/repairs"
)

// Command times (USER 2026-11-25: "perf should include things like how long
// it takes to run common things … /resume is super slow, /mcp bare is super
// slow"). Each slash command and each screen a key opens is timed from the
// keypress to the first frame that shows its result:
//
//   - a command the TUI answers itself: the frame after the Update that ran
//     it;
//   - a command that went to the engine (a control line was sent while it
//     ran): the frame after the first engine event that answers one
//     (cmdAnswers), so the time includes the engine's.
//
// The engine times its own part (cmd/dun/cmdtime.go, surface engine); the
// TUI's (surface tui) is keypress to frame. Both are cmd_time lines in the
// rounds ledger, so the Perf tab adds them up across sessions; this
// session's own are a ring here.

// engineSends counts control lines sent to the engine: a command that sent
// one waits for the engine's answer.
var engineSends atomic.Int64

// cmdAnswers are the engine events that answer a command. The ambient ones
// (token, usage, catalog, commands, settings, …) answer none.
var cmdAnswers = map[string]bool{
	"control": true, "server": true, "mcp_tools": true, "switches": true, "skill_file": true,
	"extensions": true, "plan": true, "until": true, "journal": true, "schedule": true,
	"jobs": true, "agents": true, "agent_history": true, "spend_report": true, "context_cost": true,
	"history": true, "reset": true, "clear_confirm": true, "clearing": true, "docs": true,
	"recap": true, "mcp_login": true, "request": true, "upgrade": true, "notification": true,
}

// cmdRing is how many of one command's times this session keeps.
const cmdRing = 64

// cmdWaitMax drops a command no answer came for.
const cmdWaitMax = time.Minute

type cmdPending struct {
	cmd      string
	at       time.Time
	sends    int64 // engineSends when it began
	engine   bool  // it sent a control line
	answered bool  // the engine answered
}

type cmdTimer struct {
	mu      sync.Mutex
	keyAt   time.Time // the Update being handled began
	pending *cmdPending
	times   map[string][]int64
	last    map[string]int64
	// file writes a time to the ledger; nil (a test) keeps it here only.
	file func(cmd string, ms int64)
}

var cmdTimes = cmdTimer{file: fileCmdTime}

// fileCmdTime is a cmd_time line, off the UI's goroutine.
func fileCmdTime(cmd string, ms int64) {
	go func() {
		_ = harness.NewStatsLedger(harness.DefaultStatsDir()).AppendEvent(harness.RoundEvent{
			Kind: "cmd_time", Cmd: cmd, MS: ms, Surface: "tui"})
	}()
}

// update notes when the Update being handled began: a command's time
// starts at its keypress.
func (c *cmdTimer) update(at time.Time) {
	c.mu.Lock()
	c.keyAt = at
	c.mu.Unlock()
}

// begin starts cmd's clock at the keypress.
func (c *cmdTimer) begin(cmd string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.keyAt
	if at.IsZero() {
		at = time.Now()
	}
	c.pending = &cmdPending{cmd: cmd, at: at, sends: engineSends.Load()}
}

// ran is after the command's handler: did it send to the engine.
func (c *cmdTimer) ran() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pending; p != nil && !p.engine {
		p.engine = engineSends.Load() != p.sends
	}
}

// busy reports a command waiting for its frame.
func (c *cmdTimer) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending != nil
}

// event notes an engine event: one that answers ends an engine command's
// wait (its time ends at the next frame).
func (c *cmdTimer) event(kind string) {
	if !cmdAnswers[kind] {
		return
	}
	c.mu.Lock()
	if p := c.pending; p != nil && p.engine {
		p.answered = true
	}
	c.mu.Unlock()
}

// frame is a drawn frame: the command it shows is done.
func (c *cmdTimer) frame(now time.Time) {
	c.mu.Lock()
	p := c.pending
	if p == nil || (p.engine && !p.answered && now.Sub(p.at) < cmdWaitMax) {
		c.mu.Unlock()
		return
	}
	c.pending = nil
	if now.Sub(p.at) >= cmdWaitMax {
		c.mu.Unlock()
		return
	}
	ms := now.Sub(p.at).Milliseconds()
	if c.times == nil {
		c.times, c.last = map[string][]int64{}, map[string]int64{}
	}
	ts := append(c.times[p.cmd], ms)
	if len(ts) > cmdRing {
		ts = ts[len(ts)-cmdRing:]
	}
	c.times[p.cmd], c.last[p.cmd] = ts, ms
	file := c.file
	c.mu.Unlock()
	if file != nil {
		file(p.cmd, ms)
	}
}

// session is this session's times, slowest first.
func (c *cmdTimer) session() []repairs.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []repairs.Cmd
	for cmd, ts := range c.times {
		s := append([]int64(nil), ts...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		pick := func(p int) int64 { return s[min(len(s)*p/100, len(s)-1)] }
		out = append(out, repairs.Cmd{Cmd: cmd, Surface: "tui", N: len(s), P50: pick(50), P95: pick(95), Max: s[len(s)-1], Last: c.last[cmd]})
	}
	repairs.SortCmds(out)
	return out
}

// screenName names what a model shows over the conversation, "" for none:
// a key that changes it opened a screen.
func (m *tuiModel) screenName() string {
	switch {
	case m.inspecting:
		return "inspector"
	case m.picking:
		return "resume picker"
	case m.paging:
		if v := m.statsPaging(); v != nil {
			return "/" + strings.ToLower(statsTabs[v.tab])
		}
		return "pager"
	}
	return ""
}
