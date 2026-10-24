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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fakeBinEnv  = "DUN_FAKE_BIN"  // the persona: raglit, poly-lsp
	fakeLogEnv  = "DUN_FAKE_LOG"  // where the fake appends what it was asked
	fakeHitsEnv = "DUN_FAKE_HITS" // raglit: a JSON file of search hits, read per call
	// fakeSlowEnv is milliseconds the fake sleeps before it answers
	// initialize: poly-lsp indexing a big repo before it says anything.
	fakeSlowEnv = "DUN_FAKE_SLOW_MS"
	// fakeIndexEnv makes the fake poly-lsp a build that speaks the index
	// protocol (indexstate_test.go): its `mcp --help` lists --index-budget
	// (a "limit" mode: the interim build's --index-max-files), and after
	// initialize it reports the build as the mode says (fakeIndexReport).
	// Unset, it is an older build: no flag, no report.
	fakeIndexEnv = "DUN_FAKE_INDEX"
	// fakeIndexOKEnv is a directory, or "budget=X": started with --exclude
	// for it (--index-budget X), the build is ready instead of failed.
	fakeIndexOKEnv = "DUN_FAKE_INDEX_OK"
	// fakeSetupEnv makes the fake poly-lsp send the setup notification
	// (setupstate_test.go) as the mode says (fakeSetupReport). Unset, it is
	// an older build: none.
	fakeSetupEnv = "DUN_FAKE_SETUP"
	// fakeAnywayEnv makes the fake's `mcp --help` list --index-anyway; the
	// "notproject" index mode fails without the flag, with the
	// not-a-project reason, and is ready with it.
	fakeAnywayEnv = "DUN_FAKE_ANYWAY"
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
		if slices.Contains(args, "--help") {
			fmt.Println("Usage:\n  poly-lsp-mcp mcp [flags]\n\nFlags:\n      --root string")
			switch mode := os.Getenv(fakeIndexEnv); {
			case strings.HasPrefix(mode, "limit"): // the interim build
				fmt.Println("      --exclude stringArray\n      --index-max-files int   (default 100000)")
			case mode != "":
				fmt.Println("      --exclude stringArray\n      --index-budget string   (default \"30000ms\")")
			}
			if os.Getenv(fakeAnywayEnv) != "" {
				fmt.Println("      --index-anyway")
			}
			return
		}
		fakeLogf("start POLY_LSP_HOOK=%s DUN_AGENTS_STATE=%s", os.Getenv("POLY_LSP_HOOK"), os.Getenv("DUN_AGENTS_STATE"))
		if mode := os.Getenv(fakeIndexEnv); mode != "" {
			fakeLogf("args %s", strings.Join(args, " "))
			fakeAfterInit = func(out *bufio.Writer) { fakeIndexReport(out, mode, args) }
		}
		if mode := os.Getenv(fakeSetupEnv); mode != "" {
			prev := fakeAfterInit
			fakeAfterInit = func(out *bufio.Writer) {
				if prev != nil {
					prev(out)
				}
				fakeSetupReport(out, mode)
			}
		}
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
	// manage_index answers its arguments; action "boom" is a tool error.
	"manage_index": func(a map[string]any) string {
		if a["action"] == "boom" {
			return fakeToolError + "boom"
		}
		b, _ := json.Marshal(a)
		return string(b)
	},
}

// fakeAfterInit runs once, after the initialize reply is written.
var fakeAfterInit func(out *bufio.Writer)

// fakeWorktrees is a report where .claude/worktrees holds most of the files,
// in other checkouts; fakePlain one with nothing obvious to leave out.
var (
	fakeWorktrees = map[string]any{"files": 22012, "bytes": 90 << 20, "heaviest": []map[string]any{
		{"dir": ".claude/worktrees/", "files": 21557, "bytes": 88 << 20, "share": 0.97, "nestedCheckouts": 48},
		{"dir": "cmd/", "files": 120, "bytes": 1 << 20, "share": 0.02}}}
	fakeSmall = map[string]any{"files": 455, "bytes": 2 << 20, "heaviest": []map[string]any{
		{"dir": "cmd/", "files": 120, "bytes": 1 << 20, "share": 0.5}}}
	fakePlain = map[string]any{"files": 120000, "bytes": 900 << 20, "heaviest": []map[string]any{
		{"dir": "src/", "files": 80000, "bytes": 600 << 20, "share": 0.66},
		{"dir": "docs/", "files": 40000, "bytes": 300 << 20, "share": 0.33}}}
)

// fakeIndexReport sends the index notification for mode: "ready" (a small
// tree), "nested" (ready, a checkout inside), "budget" (out of its startup
// budget, the worktrees report), "budget-plain" (out of budget, nothing
// obvious), "limit" or "limit-plain" (the interim build: failed at its file
// cap). $DUN_FAKE_INDEX_OK makes a failing mode ready when it was started
// with --exclude for that directory, or, as "budget=X", with
// --index-budget X.
func fakeIndexReport(out *bufio.Writer, mode string, args []string) {
	if ok := os.Getenv(fakeIndexOKEnv); ok != "" {
		flag, want := "--exclude", ok
		if b, isBudget := strings.CutPrefix(ok, "budget="); isBudget {
			flag, want = "--index-budget", b
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == want {
				mode = "ready"
			}
		}
	}
	data := map[string]any{"state": "ready", "ms": 40, "index": fakeSmall}
	level := "info"
	switch mode {
	case "nested":
		data["index"] = fakeWorktrees
	case "leftoutby":
		data["index"] = fakeLeftOutBy
	case "notproject":
		if !slices.Contains(args, "--index-anyway") {
			level = "error"
			data = map[string]any{"state": "failed", "ms": 3, "reason": "not a project", "reposTotal": 12,
				"repos":  []string{"a", "b", "c", "d", "e", "f", "g"},
				"error":  "not a project: /home/x is the home directory",
				"remedy": "pass --root <one project> (e.g. --root /home/x/a), or --index-anyway to index /home/x regardless"}
		}
	case "budget", "budget-plain":
		level = "error"
		rep, read, raise := fakeWorktrees, 4100, "193000ms"
		if mode == "budget-plain" {
			rep, read, raise = fakePlain, 90000, "48000ms"
		}
		data = map[string]any{"state": "failed", "ms": 30000, "index": rep, "budget": "30000ms", "read": read,
			"error": fmt.Sprintf("index build failed: index budget 30000ms spent after %d of %d indexable files", read, rep["files"]),
			"remedy": "get under budget: exclude the heavy directories above (--exclude <dir>, repeatable, relative to the root; " +
				"the repository is not changed) or add them to .gitignore. Or raise it: --index-budget " + raise + " (now 30000ms)."}
	case "limit", "limit-plain":
		level = "error"
		rep := fakeWorktrees
		if mode == "limit-plain" {
			rep = fakePlain
		}
		data = map[string]any{"state": "failed", "ms": 900, "index": rep, "limit": 10000,
			"error":  "index build failed: index limit: 22012 indexable files, limit 10000",
			"remedy": "exclude the heavy directories (--exclude <dir>, repeatable, relative to the root; the repository is not changed), or add them to .gitignore, or raise --index-max-files"}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message",
		"params": map[string]any{"level": level, "logger": "poly-lsp-mcp/index", "data": data}})
	out.Write(append(b, '\n'))
	out.Flush()
}

// fakeLeftOutBy is a report whose leftOut list holds fewer entries than
// leftOutBy counts, as poly-lsp's cap of 20 per reason makes it.
var fakeLeftOutBy = map[string]any{"files": 455, "bytes": 2 << 20,
	"leftOut": []map[string]any{
		{"dir": "node_modules/", "reason": "git-ignored"}, {"dir": "dist/", "reason": "git-ignored"},
		{"dir": "web/app.min.js", "reason": "minified"},
	},
	"leftOutBy": []map[string]any{
		{"reason": "git-ignored", "count": 8},
		{"reason": "minified", "count": 1, "hint": "index one anyway: --include <path> or include: in poly-lsp-mcp.yaml"},
		{"reason": "symlink loop", "count": 2, "hint": "the target is a directory already open on the same path; nothing is lost"},
	}}

// fakeSetupLangs are setup reports' languages, in poly-lsp's shape.
var (
	fakeGoMissing = map[string]any{"lang": "go", "files": 412,
		"lsp":     map[string]any{"name": "gopls", "found": false},
		"adds":    "resolved references (::in/::out), workspace rename, diagnostics and edit validation",
		"install": map[string]any{"cmd": "go install golang.org/x/tools/gopls@latest", "needs": []string{"go"}}}
	fakeGoFound = map[string]any{"lang": "go", "files": 412,
		"lsp": map[string]any{"name": "gopls", "found": true, "path": "/usr/bin/gopls"}}
	fakePyMissing = map[string]any{"lang": "python", "files": 12,
		"lsp":     map[string]any{"name": "pylsp", "found": false},
		"adds":    "resolved references (::in/::out), workspace rename, diagnostics and edit validation",
		"install": map[string]any{"cmd": "pipx install python-lsp-server", "needs": []string{"python"}}}
	fakeTsMissing = map[string]any{"lang": "typescript", "files": 230,
		"lsp":     map[string]any{"name": "typescript-language-server", "found": false},
		"adds":    "resolved references (::in/::out), workspace rename, diagnostics and edit validation",
		"install": map[string]any{"cmd": "npm install -g typescript-language-server typescript", "needs": []string{"node"}}}
	fakeMdOnly = map[string]any{"lang": "markdown", "files": 40}
)

// fakeSetupReport sends the setup notification for mode: "go" (gopls missing
// in a Go-heavy root), "python" (gopls found, pylsp missing for 12 Python
// files among 412 Go: must stay quiet), "two" (gopls and the TypeScript
// server missing, and pylsp for 12 files), "ok" (nothing missing).
func fakeSetupReport(out *bufio.Writer, mode string) {
	var langs []map[string]any
	switch mode {
	case "go":
		langs = []map[string]any{fakeGoMissing, fakeMdOnly}
	case "python":
		langs = []map[string]any{fakeGoFound, fakePyMissing, fakeMdOnly}
	case "two":
		langs = []map[string]any{fakeGoMissing, fakeTsMissing, fakePyMissing}
	default:
		langs = []map[string]any{fakeGoFound, fakeMdOnly}
	}
	missing, level := 0, "info"
	for _, l := range langs {
		if lsp, ok := l["lsp"].(map[string]any); ok && lsp["found"] == false {
			missing++
			level = "warning"
		}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message",
		"params": map[string]any{"level": level, "logger": "poly-lsp-mcp/setup",
			"data": map[string]any{"languages": langs, "missing": missing}}})
	out.Write(append(b, '\n'))
	out.Flush()
}

// fakeToolError starts a fake tool's answer that is an isError result.
const fakeToolError = "!error "

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
			if ms, err := strconv.Atoi(os.Getenv(fakeSlowEnv)); err == nil {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
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
			text, isErr := strings.CutPrefix(fn(msg.Params.Arguments), fakeToolError)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
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
		if msg.Method == "initialize" && fakeAfterInit != nil {
			fakeAfterInit(out)
			fakeAfterInit = nil
		}
	}
}
