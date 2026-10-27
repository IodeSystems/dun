package dun

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// What dun is, for whoever has to say (USER 2026-10-27, critical): a
// repo's rules often ask a PR or a commit to name the agent, its model and
// version. dun does not read those rules; it says what it is three ways:
//
//	the harness tool                the model asks (this file)
//	dun session <id> info [--context]   a person or a script; reads <session>.harness.json
//	DUN_SESSION_ID                  in the environment of every command dun runs
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
// <id> info. Quiet: a session without a file, or a failed write, says nothing.
func (h *Harness) writeHarnessInfo() {
	if h.cfg.SessionFile == "" || h.cfg.Parent != nil {
		return
	}
	b, err := json.MarshalIndent(h.HarnessInfo(), "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(HarnessInfoFile(h.cfg.SessionFile), append(b, '\n'), 0o600)
}

// harnessToolDef is the tool the model asks what dun is.
func harnessToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "harness"
	td.Function.Description = "What is running you: the harness (dun), its version, this session's id, the workspace, " +
		"the model and provider, the effort, the git branch and base, and the context window settings, as JSON. " +
		"Use it when a project's rules ask a commit, PR or report to name the agent, model or version: copy the " +
		"values, do not guess them."
	td.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	return td
}

// withHarness answers the harness tool locally.
func withHarness(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "harness" {
			return inner(ctx, tc)
		}
		b, _ := json.MarshalIndent(h.HarnessInfo(), "", "  ")
		out := string(b)
		if onCall != nil {
			onCall("harness", map[string]any{}, out)
		}
		return out, nil
	}
}

// identityLine introduces dun in the system prompt: its icon, its session,
// and where the rest is (USER 2026-10-27: asked for a one-character icon,
// the model answered 👷; say so, and the session).
func (h *Harness) identityLine() string {
	who := "You are dun (" + Icon + ")"
	if id := h.SessionID(); h.cfg.SessionFile != "" && id != "" {
		who += ", session " + id
	}
	if h.cfg.Version != "" {
		who += ", version " + h.cfg.Version
	}
	return "\n\n" + who + ". When a project's rules ask a commit, PR or report to name the agent, its model or " +
		"version, call harness and copy what it says."
}
