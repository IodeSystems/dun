package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/loopdetector"
)

// /loops: agentkit's loop detection on the model's output, on every client
// (USER 2026-10-31: "we want to make loop detection configurable"). loopguard.go
// is a different check and is set apart: it refuses a tool call at dispatch,
// with DUN_LOOP_REPEATS.
//
//	off    only agentkit's default guard, the same bytes back to back
//	count  the default: find loops, count them, cut only what that guard cuts
//	cut    cut a reply that continues a loop; Session.Turn retries it
//
// count keeps the guard because Install replaces it: a detector with a negative
// Tolerance would count an exact loop and let it run to the context limit.
//
// A mode or a knob applies from the next reply, on the main client and on every
// child's. DUN_LOOP_DETECT wins over the config at start. The detector's other
// settings are read from "loops" in config.json at start, and the counters
// start again when a setting changes.

// loopConfig is config.json's "loops". Zero means agentkit's default.
type loopConfig struct {
	Mode          string  `json:"mode,omitempty"`           // off, count or cut; "" is count
	Tolerance     int     `json:"tolerance,omitempty"`      // the loop count that cuts
	ToolTolerance int     `json:"tool_tolerance,omitempty"` // the same for tool calls; negative counts them only
	Window        int     `json:"window,omitempty"`         // recent sentences compared
	MinShare      float64 `json:"min_share,omitempty"`      // the share of them that must repeat
	Similarity    float64 `json:"similarity,omitempty"`     // BM25 share that makes two sentences one; negative: exact only
	ToolWindow    int     `json:"tool_window,omitempty"`    // recent tool calls compared
}

const (
	loopsOff   = "off"
	loopsCount = "count"
	loopsCut   = "cut"
)

// mode is cfg's mode with the default filled in.
func (cfg loopConfig) mode() string {
	if cfg.Mode == "" {
		return loopsCount
	}
	return cfg.Mode
}

func (cfg loopConfig) detector() *loopdetector.LoopDetector {
	c := loopdetector.Config{
		Off:           cfg.mode() == loopsOff,
		Tolerance:     cfg.Tolerance,
		ToolTolerance: cfg.ToolTolerance,
		Window:        cfg.Window,
		MinShare:      cfg.MinShare,
		Similarity:    cfg.Similarity,
		ToolWindow:    cfg.ToolWindow,
	}
	if cfg.mode() == loopsCount {
		c.Tolerance, c.ToolTolerance = -1, -1
	}
	return loopdetector.New(c)
}

// loopWatcher is the settings every client's streams read when they begin.
type loopWatcher struct {
	mu     sync.Mutex
	cfg    loopConfig
	d      *loopdetector.LoopDetector
	loaded bool
}

// loopWatch is the process's one: /loops changes every client at once.
var loopWatch = &loopWatcher{}

// load takes the config once, the first client's; DUN_LOOP_DETECT wins.
func (l *loopWatcher) load(cfg loopConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loaded {
		return
	}
	l.loaded = true
	if env := os.Getenv("DUN_LOOP_DETECT"); env != "" {
		cfg.Mode = env
	}
	if m := cfg.Mode; m != "" && m != loopsOff && m != loopsCount && m != loopsCut {
		log.Printf("dun: ignoring unknown loop detection mode %q", m)
		cfg.Mode = ""
	}
	l.cfg, l.d = cfg, cfg.detector()
}

// install makes c's streams ask l what to watch with.
func (l *loopWatcher) install(c *llm.Client, cfg loopConfig) {
	l.load(cfg)
	c.Watch = func(history []llm.Message) llm.StreamWatcher { return l.begin(c, history) }
}

func (l *loopWatcher) begin(c *llm.Client, history []llm.Message) llm.StreamWatcher {
	l.mu.Lock()
	mode, d := l.cfg.mode(), l.d
	l.mu.Unlock()
	switch mode {
	case loopsOff:
		return llm.NewStreamWatcher(c.Repetition)
	case loopsCut:
		return d.Begin(history)
	}
	return &countingWatcher{guard: llm.NewStreamWatcher(c.Repetition), session: d.Begin(history)}
}

// countingWatcher counts with the session and cuts with the guard alone.
type countingWatcher struct {
	guard   llm.StreamWatcher
	session *loopdetector.Session
}

func (w *countingWatcher) Watch(channel, delta string) *llm.RepetitionInfo {
	w.session.Watch(channel, delta)
	return w.guard.Watch(channel, delta)
}

func (w *countingWatcher) WatchToolCall(name, args string) *llm.RepetitionInfo {
	w.session.WatchToolCall(name, args)
	return nil
}

// line is the settings and the counters in words.
func (l *loopWatcher) line() string {
	l.mu.Lock()
	cfg, d := l.cfg, l.d
	l.mu.Unlock()
	var b strings.Builder
	switch cfg.mode() {
	case loopsOff:
		b.WriteString("loops: off · only the same bytes back to back are cut")
	case loopsCount:
		b.WriteString("loops: counting · only the same bytes back to back are cut")
	case loopsCut:
		fmt.Fprintf(&b, "loops: cutting at %s · tool calls at %s", tolerance(cfg.Tolerance, 3), toolTolerance(cfg))
	}
	if d == nil || cfg.mode() == loopsOff {
		return b.String()
	}
	n := d.Counters()
	fmt.Fprintf(&b, " · %d replies watched · found %s · cut %s", n.Sessions, kinds(n.Detected), kinds(n.Interrupted))
	return b.String()
}

func tolerance(n, def int) string {
	if n == 0 {
		n = def
	}
	return strconv.Itoa(n)
}

func toolTolerance(cfg loopConfig) string {
	if cfg.ToolTolerance < 0 {
		return "never"
	}
	if cfg.ToolTolerance == 0 {
		return tolerance(cfg.Tolerance, 3)
	}
	return strconv.Itoa(cfg.ToolTolerance)
}

// kinds is a counter by kind, the ones above zero, in agentkit's order.
func kinds(m map[string]int64) string {
	var out []string
	for _, k := range []string{loopdetector.KindExact, loopdetector.KindCycle, loopdetector.KindPhrase, loopdetector.KindTool} {
		if m[k] > 0 {
			out = append(out, fmt.Sprintf("%s %d", k, m[k]))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// command runs /loops's action and returns what to say and the config to save
// (nil when nothing changed).
func (l *loopWatcher) command(action string) (string, *dunConfig, error) {
	f := strings.Fields(strings.ToLower(action))
	if len(f) == 0 || f[0] == "status" {
		return l.line(), nil, nil
	}
	l.mu.Lock()
	cfg := l.cfg
	l.mu.Unlock()
	switch {
	case len(f) == 1 && (f[0] == loopsOff || f[0] == loopsCount || f[0] == loopsCut):
		cfg.Mode = f[0]
	case len(f) == 2 && f[0] == "tolerance":
		n, err := loopNumber(f[1])
		if err != nil {
			return "", nil, fmt.Errorf("tolerance: %w", err)
		}
		cfg.Tolerance = n
	case len(f) == 2 && f[0] == "tools" && f[1] == loopsCount:
		cfg.ToolTolerance = -1
	case len(f) == 2 && f[0] == "tools":
		n, err := loopNumber(f[1])
		if err != nil {
			return "", nil, fmt.Errorf("tools: %w", err)
		}
		cfg.ToolTolerance = n
	default:
		return "", nil, fmt.Errorf("usage: /loops [off|count|cut | tolerance N|default | tools N|count|default]")
	}
	l.mu.Lock()
	l.cfg, l.d, l.loaded = cfg, cfg.detector(), true
	l.mu.Unlock()
	fc := loadConfig()
	fc.Loops = cfg
	return l.line() + " (saved)", &fc, nil
}

// loopNumber is a knob's value: 2 or more, or default (0).
func loopNumber(s string) (int, error) {
	if s == "default" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 2 || n > 1000 {
		return 0, fmt.Errorf("a number from 2 to 1000, or default")
	}
	return n, nil
}
