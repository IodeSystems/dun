package main

import (
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// cmdClock times a command in the engine: from the control line's arrival
// on the reader to the first event that answers it (a ControlEvent with
// its id, a ServerEvent for a server switch, or the screen event a view
// command sends). Most handlers are detached, so the handler's own return
// says nothing; the answer is what the client waits for. The time is a
// cmd_time line in the rounds ledger (surface engine); the TUI files its
// own, keypress to frame (tui/cmdtime.go), so the two can be told apart.
type cmdClock struct {
	mu      sync.Mutex
	pending map[string]cmdStart
	group   int
	note    func(cmd string, d time.Duration)
}

type cmdStart struct {
	cmd   string
	at    time.Time
	group int
}

// cmdWait is how long an answer is waited for; past it the start is dropped.
const cmdWait = 2 * time.Minute

// start waits for an answer under each of keys, one command: the first
// answer under any of them is its time.
func (c *cmdClock) start(cmd string, at time.Time, keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[string]cmdStart{}
	}
	c.group++
	for _, k := range keys {
		c.pending[k] = cmdStart{cmd: cmd, at: at, group: c.group}
	}
}

// also adds key to the command started at at (a slash routed on).
func (c *cmdClock) also(key string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.pending {
		if s.at.Equal(at) {
			c.pending[key] = s
			return
		}
	}
}

// answered is the emitter's hook: an event that answers a pending command
// files its time.
func (c *cmdClock) answered(payload any) {
	key := answerKey(payload)
	if key == "" {
		return
	}
	c.mu.Lock()
	s, ok := c.pending[key]
	now := time.Now()
	if ok {
		for k, o := range c.pending {
			if o.group == s.group || now.Sub(o.at) > cmdWait {
				delete(c.pending, k)
			}
		}
	}
	note := c.note
	c.mu.Unlock()
	if ok && note != nil && now.Sub(s.at) <= cmdWait {
		note(s.cmd, now.Sub(s.at))
	}
}

// answerKey is the key an event answers under; "" for one that answers no
// command.
func answerKey(payload any) string {
	switch p := payload.(type) {
	case *proto.ControlEvent:
		return p.ID
	case *proto.ServerEvent:
		if p.Background {
			return ""
		}
		return "srv:" + p.ID
	case *proto.McpTools:
		return "mcptools"
	case *proto.Switches:
		return "switches"
	case *proto.SkillFile:
		return "skillfile"
	case *proto.Extensions:
		return "ext"
	}
	return ""
}

// ctrlCmdName names a control command the way a person typed it: a slash
// line by its command word, any other by its id.
func ctrlCmdName(id, action string) string {
	if id == "slash" {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(action), "/"))
		if len(f) == 0 {
			return ""
		}
		return "/" + strings.ToLower(f[0])
	}
	return "/" + id
}
