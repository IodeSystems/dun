package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// What dun is, three ways: the session tool, the file dun session info
// reads, and the system prompt's line (USER 2026-10-27).
func TestHarnessInfo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	ws := t.TempDir()
	sf := filepath.Join(t.TempDir(), "20261027-101500.jsonl")
	h := &Harness{cfg: Config{Workspace: ws, SessionFile: sf, Version: "v0.1.3",
		Provider: "openrouter", Endpoint: Endpoint{Model: "deepseek/x"}, Exec: &HostShell{Dir: ws}}}
	h.effort.Store(EffortHigh)
	in := h.HarnessInfo()
	if in.Name != "dun" || in.Icon != "👷" || in.SessionID != "20261027-101500" || in.Version != "v0.1.3" ||
		in.Model != "deepseek/x" || in.Provider != "openrouter" || in.Effort != "high" || in.Workspace != ws || in.Context == nil {
		t.Errorf("info: %+v", in)
	}
	// The line names the env var, not the path: no session id in the system
	// prompt (the same text in every session, for the cache).
	line := h.identityLine()
	if !strings.Contains(line, "$DUN_HARNESS") || strings.Contains(line, "20261027") {
		t.Errorf("identity line: %s", line)
	}
	// "You are dun" is said once, in the preamble, with the icon.
	if strings.Contains(line, "You are dun") || !strings.Contains(systemPreamble, "You are dun (👷)") {
		t.Errorf("identity said twice or icon missing: %q / %q", line, systemPreamble)
	}
	h.writeHarnessInfo()
	b, err := os.ReadFile(HarnessInfoFile(sf))
	if err != nil || !strings.Contains(string(b), `"session_id": "20261027-101500"`) {
		t.Errorf("file: %v %s", err, b)
	}
	// exec reads it through $DUN_HARNESS, as the model would.
	sh := h.withExecEnv(h.cfg.Exec).(*HostShell)
	defer sh.Close()
	if r := sh.Run(context.Background(), `cat "$DUN_HARNESS"`, nil); !strings.Contains(r.Output, `"session_id": "20261027-101500"`) {
		t.Errorf("cat $DUN_HARNESS: %+v", r)
	}
}

// The record a commit's attribution is read from must be right when read.
// /model use and /login change the saved default; the engine then restarts
// on the same session file, and Start rewrites the record with the new model
// (a running harness's endpoint never changes). /effort rewrites it
// (SetEffort).
func TestHarnessInfo_ModelSwitchLeavesRunningRecordTrue(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{}, &asked)
	sf := h.cfg.SessionFile
	read := func() HarnessInfo {
		var in HarnessInfo
		b, err := os.ReadFile(HarnessInfoFile(sf))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	if in := read(); in.Model != "big" {
		t.Fatalf("model %q", in.Model)
	}
	for _, op := range []dunconfig.ProviderOp{
		{Op: "login", Name: "other", URL: "http://other.test/v1"},
		{Op: "use", Name: "other", Model: "small"},
	} {
		if _, err := dunconfig.ApplyProviderOp(op); err != nil {
			t.Fatal(err)
		}
	}
	if in := read(); in.Model != "big" || h.cfg.Endpoint.Model != "big" {
		t.Errorf("the running session's model changed: record %q, endpoint %q", in.Model, h.cfg.Endpoint.Model)
	}
	if err := h.SetEffort(EffortLow); err != nil {
		t.Fatal(err)
	}
	if in := read(); in.Effort != EffortLow {
		t.Errorf("effort after /effort: %q", in.Effort)
	}
	// The restart: a new engine on the same session file, on the new model.
	h.Close()
	h2, err := Start(context.Background(), Config{
		Workspace: h.cfg.Workspace, Servers: []mcpserver.Server{}, Client: &scriptRunner{},
		Endpoint:    Endpoint{URL: "http://other.test/v1", Model: "small"},
		SessionFile: sf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	if in := read(); in.Model != "small" || in.Effort != EffortLow {
		t.Errorf("after the restart: model %q effort %q", in.Model, in.Effort)
	}
}

// A sub-agent runs on its own model and effort. Its commands' $DUN_SESSION_ID
// names its own record, not the parent's, so dun session attribution cannot
// print the parent's model for it.
func TestHarnessInfo_ChildCommandsNameTheChild(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_SESSION_ID", "the-parent-from-the-process")
	dir := t.TempDir()
	kid := &scriptRunner{rounds: []roundFn{textRound("done")}}
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: &scriptRunner{},
		Endpoint: Endpoint{URL: "http://claude.test/v1", Model: "big"},
		Exec:     &HostShell{Dir: dir},
		ClientFor: func(provider, model string) (ChildClient, error) {
			return ChildClient{Runner: kid, Endpoint: Endpoint{URL: "http://local.test/v1", Model: model}}, nil
		},
		Providers:   []string{"claude", "local"},
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	sa, err := h.spawnAgent(context.Background(), "say done", "local", "qwen3-8b", EffortLow, "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)

	say := func(e ExecBackend) string {
		r := e.Run(context.Background(), `printf %s "$DUN_SESSION_ID"`, nil)
		return strings.TrimSpace(r.Output)
	}
	if got := say(h.cfg.Exec); got != "s" {
		t.Errorf("parent's commands: $DUN_SESSION_ID = %q, want s", got)
	}
	got := say(sa.h.cfg.Exec)
	if got != "s.sub1" {
		t.Fatalf("child's commands: $DUN_SESSION_ID = %q, want s.sub1", got)
	}
	var in HarnessInfo
	b, err := os.ReadFile(HarnessInfoFile(sa.h.cfg.SessionFile))
	if err != nil {
		t.Fatalf("a child keeps its own harness record: %v", err)
	}
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	if in.SessionID != got || in.Model != "qwen3-8b" || in.Provider != "local" || in.Effort != EffortLow {
		t.Errorf("child record %+v", in)
	}
	if p, err := os.ReadFile(HarnessInfoFile(h.cfg.SessionFile)); err != nil || !strings.Contains(string(p), `"model": "big"`) {
		t.Errorf("parent record %v %s", err, p)
	}
}

// Commands read the agent's facts when they start: the persistent shell's
// export line re-reads liveEnv at each command (so /effort shows at once),
// the stateless backends read it per call, and a child's are its own.
func TestHarnessInfo_LiveEnvFollowsTheSession(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	kid := &scriptRunner{rounds: []roundFn{textRound("done")}}
	sh := &HostShell{Dir: dir}
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: &scriptRunner{}, Version: "v9.9.9",
		Endpoint: Endpoint{URL: "http://claude.test/v1", Model: "big"}, Exec: sh,
		ClientFor: func(provider, model string) (ChildClient, error) {
			return ChildClient{Runner: kid, Endpoint: Endpoint{URL: "http://local.test/v1", Model: model}}, nil
		},
		Providers: []string{"claude", "local"}, SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	const cmd = `printf '%s|%s|%s|%s' "$DUN_MODEL_ID" "$DUN_EFFORT" "$DUN_HARNESS_VERSION" "$DUN_HARNESS_NAME"`
	run := func(e ExecBackend) ExecResult {
		r := e.Run(context.Background(), cmd, nil)
		r.Output = strings.TrimSpace(r.Output)
		return r
	}
	if r := run(h.cfg.Exec); r.Output != "big|default|v9.9.9|dun" || r.Code != 0 {
		t.Errorf("first: %+v", r)
	}
	if err := h.SetEffort(EffortHigh); err != nil {
		t.Fatal(err)
	}
	// No in-session model switch exists; this changes the field the export
	// line reads, to show liveEnv is read again at each command.
	h.cfg.Endpoint.Model = "bigger"
	if r := run(h.cfg.Exec); r.Output != "bigger|high|v9.9.9|dun" {
		t.Errorf("after the change, same shell: %+v", r)
	}
	// The export line breaks neither the exit status nor the cwd reset.
	if r := h.cfg.Exec.Run(context.Background(), `cd / ; false`, nil); r.Code != 1 || strings.Contains(r.Output, "DUN_") {
		t.Errorf("status/output: %+v", r)
	}
	if r := h.cfg.Exec.Run(context.Background(), `pwd`, nil); strings.TrimSpace(r.Output) != dir {
		t.Errorf("cwd: %+v", r)
	}
	// A value with a quote is carried whole, not run.
	h.cfg.Endpoint.Model = "a'b $(echo x)"
	if r := h.cfg.Exec.Run(context.Background(), `printf %s "$DUN_MODEL_ID"`, nil); strings.TrimSpace(r.Output) != "a'b $(echo x)" {
		t.Errorf("quoting: %+v", r)
	}
	h.cfg.Endpoint.Model = "big"
	// Stateless backend.
	he := h.withExecEnv(HostExec{Dir: dir})
	if r := run(he); r.Output != "big|high|v9.9.9|dun" {
		t.Errorf("HostExec: %+v", r)
	}
	// A child's own.
	sa, err := h.spawnAgent(context.Background(), "x", "local", "qwen3-8b", EffortLow, "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if r := run(sa.h.cfg.Exec); r.Output != "qwen3-8b|low|v9.9.9|dun" {
		t.Errorf("child: %+v", r)
	}
	if r := run(h.cfg.Exec); r.Output != "big|high|v9.9.9|dun" {
		t.Errorf("parent after child: %+v", r)
	}
}

// shell eval carries a live, reserved `dun`.
func TestHarnessInfo_EvalCarriesLiveDun(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{}, &asked)
	var got map[string]any
	inner := callFn(func(_ context.Context, _, _ string, args map[string]any) (string, error) {
		got = args
		return "", nil
	})
	c := dunVarsCaller{inner, h}
	call := func(args map[string]any) map[string]any {
		got = nil
		if _, err := c.CallTool(context.Background(), "shell", "eval", args); err != nil {
			t.Fatal(err)
		}
		return got["vars"].(map[string]any)["dun"].(map[string]any)
	}
	if d := call(map[string]any{"code": "1"}); d["model_id"] != "big" || d["harness"] != "dun" || d["harness_version"] != "dev" {
		t.Errorf("dun: %v", d)
	}
	if err := h.SetEffort(EffortHigh); err != nil {
		t.Fatal(err)
	}
	mine := map[string]any{"dun": "mine", "x": 1.0}
	d := call(map[string]any{"code": "1", "vars": mine})
	if d["effort"] != "high" {
		t.Errorf("after /effort: %v", d)
	}
	if got["vars"].(map[string]any)["x"] != 1.0 || mine["dun"] != "mine" {
		t.Errorf("the model's vars were not kept, or its map was written: %v", got["vars"])
	}
	// vars that is not an object is an error, not replaced.
	got = nil
	if _, err := c.CallTool(context.Background(), "shell", "eval", map[string]any{"code": "1", "vars": []any{1.0}}); err == nil || got != nil {
		t.Errorf("non-object vars: err %v, forwarded %v", err, got)
	}
	// Another tool is untouched.
	got = nil
	c.CallTool(context.Background(), "shell", "help", map[string]any{"a": 1})
	if _, ok := got["vars"]; ok {
		t.Errorf("vars added to another tool: %v", got)
	}
}

type callFn func(ctx context.Context, serverID, tool string, args map[string]any) (string, error)

func (f callFn) CallTool(ctx context.Context, serverID, tool string, args map[string]any) (string, error) {
	return f(ctx, serverID, tool, args)
}
