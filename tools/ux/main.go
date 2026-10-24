// Command ux is bin/ux's helper: the stand-ins a walk needs, and the step
// that turns a recording into the run directory a persona reads.
//
//	ux llm --port-file F [--log L]   an OpenAI-compatible endpoint (stub.go)
//	ux mcp                           a stdio MCP server (stub.go); also run as `notes-mcp`
//	ux assemble …                    write run.json, BRIEF.md, ACTIONS.md (assemble.go)
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	// Installed under the name a walk types into dun, it is the MCP server.
	if filepath.Base(os.Args[0]) == "notes-mcp" {
		runMCP(os.Stdin, os.Stdout)
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ux llm|mcp|assemble [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "llm":
		runLLM(os.Args[2:])
	case "mcp":
		runMCP(os.Stdin, os.Stdout)
	case "assemble":
		if err := runAssemble(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ux assemble:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "ux: unknown command", os.Args[1])
		os.Exit(2)
	}
}
