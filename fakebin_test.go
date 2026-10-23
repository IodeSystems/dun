package dun

// Fake raglit and poly-lsp-mcp binaries, so a test can drive the harness the
// way a real session does: start a server by its id, let it be indexed,
// searched and armed, and read back what dun asked of it.
//
// The binary is THIS test binary re-executed (TestMain checks fakeBinEnv),
// behind a small sh wrapper named after the real command — dun picks the
// raglit CLI by the command's base name. What the fake was asked is appended
// to a log file, one line per CLI run, tool call or start.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fakeBinEnv  = "DUN_FAKE_BIN"  // the persona: raglit, poly-lsp
	fakeLogEnv  = "DUN_FAKE_LOG"  // where the fake appends what it was asked
	fakeHitsEnv = "DUN_FAKE_HITS" // raglit: a JSON file of search hits, read per call
)

// fakeBin writes dir/name, a wrapper that runs this test binary as persona,
// and returns its path and the log it writes.
func fakeBin(t *testing.T, name, persona string, env ...string) (cmd, logPath string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	vars := append([]string{fakeBinEnv + "=" + persona, fakeLogEnv + "=" + logPath}, env...)
	for i, v := range vars {
		k, val, _ := strings.Cut(v, "=")
		vars[i] = k + "='" + val + "'"
	}
	cmd = filepath.Join(dir, name)
	script := "#!/bin/sh\n" + strings.Join(vars, " ") + " exec '" + exe + "' \"$@\"\n"
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, logPath
}

// fakeLog is the fake's log so far, one entry per line.
func fakeLog(path string) []string {
	b, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func fakeLogf(format string, a ...any) {
	f, err := os.OpenFile(os.Getenv(fakeLogEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", a...)
}

// fakeBinMain is the fake's whole life: a CLI run logs its argv and exits;
// a server run logs how it was started and serves MCP on stdio.
func fakeBinMain(persona string) {
	args := os.Args[1:]
	switch persona {
	case "raglit":
		if len(args) == 0 || args[0] != "serve" {
			fakeLogf("raglit %s", strings.Join(args, " "))
			return
		}
		fakeLogf("serve %s", strings.Join(args[1:], " "))
		serveFake(raglitFakeTools)
	case "poly-lsp":
		fakeLogf("start POLY_LSP_HOOK=%s DUN_AGENTS_STATE=%s", os.Getenv("POLY_LSP_HOOK"), os.Getenv("DUN_AGENTS_STATE"))
		serveFake(map[string]func(map[string]any) string{
			"node_query": func(map[string]any) string { return "[]" },
		})
	}
}

// raglitFakeTools are the raglit tools dun calls: search answers from the
// hits file, get_document reads the document from disk.
var raglitFakeTools = map[string]func(map[string]any) string{
	"search": func(map[string]any) string {
		b, err := os.ReadFile(os.Getenv(fakeHitsEnv))
		if err != nil {
			return `{"hits":[]}`
		}
		return `{"hits":` + string(b) + `}`
	},
	"get_document": func(a map[string]any) string {
		p, _ := a["path"].(string)
		b, err := os.ReadFile(p)
		if err != nil {
			return "no document"
		}
		out, _ := json.Marshal(map[string]any{"text": string(b), "truncated": false})
		return string(out)
	},
	"ingest": func(a map[string]any) string { return `{"state":"pending"}` },
}

// serveFake answers MCP over stdio with the given tools.
func serveFake(tools map[string]func(map[string]any) string) {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil || len(msg.ID) == 0 {
			continue // a notification
		}
		var result any
		var rpcErr any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			var list []map[string]any
			for name := range tools {
				list = append(list, map[string]any{"name": name, "description": "fake " + name,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			fn, ok := tools[msg.Params.Name]
			if !ok {
				rpcErr = map[string]any{"code": -32602, "message": "no tool " + msg.Params.Name}
				break
			}
			a, _ := json.Marshal(msg.Params.Arguments)
			fakeLogf("call %s %s", msg.Params.Name, a)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": fn(msg.Params.Arguments)}}}
		case "ping":
			result = map[string]any{}
		default:
			rpcErr = map[string]any{"code": -32601, "message": "no method " + msg.Method}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		out.Write(append(b, '\n'))
		out.Flush()
	}
}
