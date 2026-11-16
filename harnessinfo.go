package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/fsutil"
)

// What dun is, for whoever has to say (USER 2026-10-27, critical): a
// repo's rules often ask a PR or a commit to name the agent, its model and
// version. dun does not read those rules; it says what it is three ways:
//
//	the session tool                the model asks (session_tool.go)
//	dun session <id> info [--context]   a person or a script; reads <session>.harness.json
//	DUN_SESSION_ID                  in the environment of every command dun runs
//	dun session [<id>] attribution  the agent line for a commit or PR: harness, version, model, effort
//
// and the system prompt introduces it as dun (👷) with its session id.

// Icon is dun's when one character is wanted.
const Icon = "👷"

// HarnessInfo is what dun is in this session.
type HarnessInfo struct {
	Name      string       `json:"name"`
	Icon      string       `json:"icon"`
	Version   string       `json:"version"`
	Harness   string       `json:"harness"`
	SessionID string       `json:"session_id"`
	Workspace string       `json:"workspace"`
	Model     string       `json:"model"`
	Provider  string       `json:"provider,omitempty"`
	Effort    string       `json:"effort"`
	Git       *GitInfo     `json:"git,omitempty"`
	Context   *ContextInfo `json:"context,omitempty"`
	Updated   string       `json:"updated"`
}

// GitInfo is the workspace's branch, the branch it is based on, and the
// worktree when dun made one.
type GitInfo struct {
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

// ContextInfo is /context config's settings and how full the window is.
type ContextInfo struct {
	Max           int    `json:"max"`
	MaxFrom       string `json:"max_from,omitempty"`
	CompactBuffer int    `json:"compact_buffer"`
	ToolLOD       int    `json:"tool_lod"`
	RecapNudge    int    `json:"recap_nudge"`
}

// SessionID is this session's id: its log's name; "" in memory.
func (h *Harness) SessionID() string {
	return strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
}

// HarnessInfo is what dun is now.
func (h *Harness) HarnessInfo() HarnessInfo {
	v := h.cfg.Version
	if v == "" {
		v = "dev"
	}
	max, from, buf, lod, nudge := h.ContextSettings()
	in := HarnessInfo{Name: "dun", Icon: Icon, Version: v, Harness: "dun", Workspace: h.cfg.Workspace,
		Model: h.cfg.Endpoint.Model, Provider: h.cfg.Provider, Effort: h.Effort(),
		Context: &ContextInfo{Max: max, MaxFrom: from, CompactBuffer: buf, ToolLOD: lod, RecapNudge: nudge},
		Updated: time.Now().UTC().Format(time.RFC3339)}
	if h.cfg.SessionFile != "" {
		in.SessionID = h.SessionID()
	}
	in.Git = h.gitInfo()
	return in
}

// gitInfo is the workspace's branch and base; nil outside a repo.
func (h *Harness) gitInfo() *GitInfo {
	if wt := h.Worktree(); wt != nil && wt.Branch != "" {
		return &GitInfo{Branch: wt.Branch, Base: wt.BaseBranch, Worktree: wt.Path, Commit: gitOut(wt.Path, "rev-parse", "--short", "HEAD")}
	}
	dir := h.cfg.Workspace
	branch := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return nil
	}
	base := strings.TrimPrefix(gitOut(dir, "rev-parse", "--abbrev-ref", "origin/HEAD"), "origin/")
	return &GitInfo{Branch: branch, Base: base, Commit: gitOut(dir, "rev-parse", "--short", "HEAD")}
}

func gitOut(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// HarnessInfoFile is where a session's facts are kept, beside its log.
func HarnessInfoFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".harness.json"
}

// writeHarnessInfo keeps <session>.harness.json current, for dun session
// <id> info and attribution. A child has its own (<parent>.sub<N>.harness.json):
// its model and effort are not its parent's, and its commands' $DUN_SESSION_ID
// names it (withExecEnv). Quiet: a session without a file, or a failed write,
// says nothing.
func (h *Harness) writeHarnessInfo() {
	if h.cfg.SessionFile == "" {
		return
	}
	b, err := json.MarshalIndent(h.HarnessInfo(), "", "  ")
	if err != nil {
		return
	}
	_ = fsutil.WriteAtomic(HarnessInfoFile(h.cfg.SessionFile), append(b, '\n'), 0o600)
	// The model's copy, $DUN_HARNESS (harnessTmpPath): in the isolation tmp,
	// so exec reads it on the host and under --docker alike.
	if p := h.harnessTmpPath(); p != "" {
		_ = fsutil.WriteAtomic(p, append(b, '\n'), 0o600)
	}
}

// harnessTmpPath is the model's copy of the harness info, $DUN_HARNESS in
// exec's environment (USER 2026-11-06: harness info replaces the session tool
// with one line). "" for a child or a session without an isolation tmp.
func (h *Harness) harnessTmpPath() string {
	if h.cfg.SessionFile == "" || h.cfg.Parent != nil {
		return ""
	}
	tmp := h.isoTmp()
	if tmp == "" {
		return ""
	}
	return filepath.Join(tmp, "harness.json")
}

// withExecEnv gives exec's commands $DUN_HARNESS, $DUN_SESSION_ID and liveEnv's. Per backend, not the
// process environment: a child harness runs in the same process.
func (h *Harness) withExecEnv(e ExecBackend) ExecBackend {
	var env []string
	if p := h.harnessTmpPath(); p != "" {
		env = append(env, "DUN_HARNESS="+p)
	}
	// $DUN_SESSION_ID is per process (main.go), so a child's commands would
	// name its parent, and dun session attribution would print the parent's
	// model. A child with no log names no session rather than the parent's.
	if h.cfg.SessionFile != "" {
		env = append(env, "DUN_SESSION_ID="+h.SessionID())
	} else if h.cfg.Parent != nil {
		env = append(env, "DUN_SESSION_ID=")
	}
	switch b := e.(type) {
	case *HostShell:
		b.Env, b.Live = env, h.liveEnv
	case HostExec:
		b.Env, b.Live = env, h.liveEnv
		return b
	case DockerExec:
		b.Env, b.Live = env, h.liveEnv
		return b
	}
	return e
}

// liveEnv is what a command is told about the agent running it, read when
// the command starts: $DUN_MODEL_ID, $DUN_EFFORT, $DUN_HARNESS_VERSION,
// $DUN_HARNESS_NAME, $DUN_PROVIDER. A child's are its own. ($DUN_HARNESS is
// taken: it names the harness.json file.) A fact that is not known is left
// out, not guessed. A long-lived child process (an MCP server, a job that
// outlives its turn) has only its start-time values; it runs
// `dun session attribution` for the current ones.
func (h *Harness) liveEnv() []string {
	v := h.cfg.Version
	if v == "" {
		v = "dev"
	}
	var env []string
	for _, kv := range [][2]string{{"DUN_HARNESS_NAME", "dun"}, {"DUN_HARNESS_VERSION", v},
		{"DUN_MODEL_ID", h.cfg.Endpoint.Model}, {"DUN_PROVIDER", h.cfg.Provider}, {"DUN_EFFORT", h.Effort()}} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}

// identityLine says where dun's harness info is (USER 2026-10-27: asked for
// a one-character icon, the model answered 👷). The icon itself is in the
// preamble (systemPreamble), so "You are dun" is said once.
func (h *Harness) identityLine() string {
	// A file named by an env var, not a tool (USER 2026-11-06): no schema on
	// every request. No session id or version in the line itself, so the
	// prompt's text is the same in every session (the provider's cache) and
	// nothing here goes stale. Only where exec can read it.
	// Not under --non-interactive: no project rules ask a benchmark or a
	// script run to name the agent in a commit (USER 2026-11-07, prompt trim).
	if h.harnessTmpPath() == "" || h.cfg.Exec == nil || h.cfg.NonInteractive {
		return ""
	}
	return "\n\nHarness info (dun version, session, model, effort, branch) is the JSON file $DUN_HARNESS; " +
		"copy from it when a project's rules ask a commit, PR or report to name the agent, model or version."
}

// dunVars is the `dun` constant every shell eval carries: the same facts as
// liveEnv, read when the call is made, so a child's are its own.
func (h *Harness) dunVars() map[string]any {
	v := h.cfg.Version
	if v == "" {
		v = "dev"
	}
	m := map[string]any{"harness": "dun", "harness_version": v, "model_id": h.cfg.Endpoint.Model, "effort": h.Effort()}
	if h.cfg.Provider != "" {
		m["provider"] = h.cfg.Provider
	}
	return m
}

// dunVarsCaller binds `dun` into the shell server's eval calls (mcpshell's
// eval takes `vars`, per-call constants). Reserved: a `dun` the model passes
// is overwritten. The other vars and the model's args are left alone.
type dunVarsCaller struct {
	toolCaller
	h *Harness
}

func (c dunVarsCaller) CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error) {
	if serverID == "shell" && toolName == "eval" {
		out := make(map[string]any, len(args)+1)
		for k, v := range args {
			out[k] = v
		}
		vars := map[string]any{}
		if raw, present := out["vars"]; present && raw != nil {
			old, ok := raw.(map[string]any)
			if !ok {
				return "", fmt.Errorf("eval: vars is an object of name: value; got %T", raw)
			}
			for k, v := range old {
				vars[k] = v
			}
		}
		vars["dun"] = c.h.dunVars()
		out["vars"] = vars
		args = out
	}
	return c.toolCaller.CallTool(ctx, serverID, toolName, args)
}
