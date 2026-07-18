// Command dun (Slice 1, headless): compose poly-lsp-mcp + mcpshell + raglit into
// an agentkit loop and work a task in a workspace, streaming to the terminal.
// The Bubble Tea TUI + Docker/worktree isolation come next (see plan/plan.md).
//
//	dun [--workspace DIR] [--model M] "your task"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

func main() {
	url := flag.String("url", "https://llm.iodesystems.com", "LLM base URL")
	model := flag.String("model", "ternary-bonsai-27b", "chat model (must support tool calls)")
	key := flag.String("key", os.Getenv("DUN_LLM_KEY"), "API key (or $DUN_LLM_KEY)")
	ws := flag.String("workspace", ".", "workspace directory")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()
	task := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, `usage: dun [--workspace DIR] [--model M] "task"`)
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

	fmt.Fprintf(os.Stderr, "dun: spawning tool servers for %s …\n", absWS)
	h, err := dun.Start(ctx, dun.Config{
		Workspace:  absWS,
		RaglitHome: raglitHome,
		Client:     llm.NewClient(*url, *key, *model),
		OnToken:    func(s string) { fmt.Print(s) },
		OnToolCall: func(tool string, args map[string]any, result string) {
			fmt.Fprintf(os.Stderr, "\n  ⚙ %s(%s) → %s\n", tool, shortArgs(args), clip(oneLine(result), 200))
		},
	})
	if err != nil {
		fatal(err)
	}
	defer h.Close()

	fmt.Fprintf(os.Stderr, "dun: %d tools ready: %s\n\ntask: %s\n\n",
		len(h.ToolNames()), strings.Join(h.ToolNames(), ", "), task)

	res, err := h.Ask(ctx, task)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\n\n--- done (%d tokens) ---\n", res.Usage.Total)
	_ = res
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
