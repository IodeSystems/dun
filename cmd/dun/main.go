// Command dun (Slice 1): compose poly-lsp-mcp + mcpshell + raglit into an
// agentkit loop and work a task in a workspace.
//
//	dun [--workspace DIR] [--model M] "your task"     human-readable stream
//	dun -p [--workspace DIR] ["first task"]           programmatic: line-delimited
//	                                                  JSON events in/out
//
// The Bubble Tea TUI (Slice 2) is a CONSUMER of the -p event stream.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

func main() {
	url := flag.String("url", "https://llm.iodesystems.com", "LLM base URL")
	model := flag.String("model", "ternary-bonsai-27b", "chat model (must support tool calls)")
	key := flag.String("key", os.Getenv("DUN_LLM_KEY"), "API key (or $DUN_LLM_KEY)")
	ws := flag.String("workspace", ".", "workspace directory")
	prog := flag.Bool("p", false, "programmatic mode: emit + read line-delimited JSON events")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout")
	flag.Parse()
	firstTask := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if firstTask == "" && !*prog {
		fmt.Fprintln(os.Stderr, `usage: dun [--workspace DIR] "task"   (or: dun -p  for JSON events)`)
		os.Exit(2)
	}

	absWS, err := filepath.Abs(*ws)
	if err != nil {
		fatal(err)
	}
	raglitHome, err := os.MkdirTemp("", "dun-raglit-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(raglitHome)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var em *emitter
	var cfg dun.Config
	cfg = dun.Config{
		Workspace:  absWS,
		RaglitHome: raglitHome,
		Client:     llm.NewClient(*url, *key, *model),
	}
	if *prog {
		em = &emitter{}
		cfg.OnToken = func(s string) { em.emit(event{"type": "token", "text": s}) }
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			em.emit(event{"type": "tool_call", "tool": tool, "args": args})
			em.emit(event{"type": "tool_result", "tool": tool, "result": result})
		}
	} else {
		cfg.OnToken = func(s string) { fmt.Print(s) }
		cfg.OnToolCall = func(tool string, args map[string]any, result string) {
			fmt.Fprintf(os.Stderr, "\n  ⚙ %s(%s) → %s\n", tool, shortArgs(args), clip(oneLine(result), 200))
		}
		fmt.Fprintf(os.Stderr, "dun: spawning tool servers for %s …\n", absWS)
	}

	h, err := dun.Start(ctx, cfg)
	if err != nil {
		if em != nil {
			em.emit(event{"type": "error", "error": err.Error()})
		}
		fatal(err)
	}
	defer h.Close()

	if *prog {
		runProgrammatic(ctx, h, em, firstTask)
		return
	}
	runHuman(ctx, h, firstTask)
}

// runHuman streams a single task in human-readable form.
func runHuman(ctx context.Context, h *dun.Harness, task string) {
	fmt.Fprintf(os.Stderr, "dun: %d tools ready: %s\n\ntask: %s\n\n",
		len(h.ToolNames()), strings.Join(h.ToolNames(), ", "), task)
	res, err := h.Ask(ctx, task)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\n\n--- done (%d tokens) ---\n", res.Usage.Total)
}

// runProgrammatic drives dun over line-delimited JSON events: emit `ready`, run
// the first task if given, then read {"type":"user","content":...} events from
// stdin (one JSON per line) until EOF, running a turn per message.
func runProgrammatic(ctx context.Context, h *dun.Harness, em *emitter, firstTask string) {
	em.emit(event{"type": "ready", "tools": h.ToolNames()})
	if firstTask != "" {
		turn(ctx, h, em, firstTask)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var in struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			em.emit(event{"type": "error", "error": "bad input event: " + err.Error()})
			continue
		}
		switch in.Type {
		case "user":
			turn(ctx, h, em, in.Content)
		case "stop", "quit":
			return
		default:
			em.emit(event{"type": "error", "error": "unknown input event type: " + in.Type})
		}
	}
}

func turn(ctx context.Context, h *dun.Harness, em *emitter, task string) {
	res, err := h.Ask(ctx, task)
	if err != nil {
		em.emit(event{"type": "error", "error": err.Error()})
		return
	}
	em.emit(event{"type": "message", "role": "assistant", "content": res.Reply})
	em.emit(event{"type": "usage", "total": res.Usage.Total, "active": res.Usage.Active})
	em.emit(event{"type": "done"})
}

type event map[string]any

// emitter writes one JSON event per line to stdout, serialized (tokens stream
// from the same goroutine as turns today, but the mutex keeps it safe if that
// changes).
type emitter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (e *emitter) emit(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		e.enc = json.NewEncoder(os.Stdout)
	}
	_ = e.enc.Encode(ev)
}

func shortArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, clip(fmt.Sprint(args[k]), 40)))
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "dun: %v\n", err)
	os.Exit(1)
}
