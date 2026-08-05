package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// The mount tool — ask the user to mount a host path inside the Docker
// container. This is how a session that discovers an unmounted dependency
// (e.g. a go.mod replace pointing to ../agentkit) gets access to it without
// requiring the user to edit dun.json by hand.
//
// The agent calls mount{source, name}; the turn pauses until the user approves
// or denies. On approval the mount is added to the running session: the
// DockerExec backend is reconfigured with the new volume, and a symlink is
// created in the worktree parent directory so go.mod replace resolves.

// MountFunc asks the user whether to mount a host path inside the container.
// source is the host path (absolute or relative to repo root), name is the
// mount point name (the path will be available at /<name> inside the container
// and as a symlink named <name> in the worktree parent). It blocks until the
// user answers or ctx is done.
type MountFunc func(ctx context.Context, source, name string) (approved bool, err error)

func mountToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "mount"
	td.Function.Description = "Ask the user to mount a host directory inside the Docker container. " +
		"Use this when a build or tool needs access to a path outside the worktree " +
		"(e.g. a go.mod replace directive pointing to a sibling module). " +
		"The user must approve before the mount is added. " +
		"Returns whether the mount was approved and is now active."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{
				"type": "string",
				"description": "the host path to mount (absolute, or relative to the repo root like ../agentkit)",
			},
			"name": map[string]any{
				"type": "string",
				"description": "the mount name — the path will be available at /<name> inside the container " +
					"and as a symlink named <name> in the worktree parent directory",
			},
		},
		"required": []string{"source", "name"},
	}
	return td
}

// withMount wraps a dispatcher so the mount tool is handled locally; everything
// else routes onward.
func withMount(inner agent.ToolDispatcher, ask MountFunc, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "mount" {
			return inner(ctx, tc)
		}
		var args struct {
			Source string `json:"source"`
			Name   string `json:"name"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		source := strings.TrimSpace(args.Source)
		name := strings.TrimSpace(args.Name)
		if source == "" {
			return "ERROR: mount requires a source path", nil
		}
		if name == "" {
			return "ERROR: mount requires a name", nil
		}
		ok, err := ask(ctx, source, name)
		if err != nil {
			return "ERROR: could not get an answer: " + err.Error(), nil
		}
		result := fmt.Sprintf("mount denied: %s", source)
		if ok {
			result = fmt.Sprintf("mount approved: %s → /%s", source, name)
		}
		if onCall != nil {
			onCall("mount", map[string]any{"source": source, "name": name}, result)
		}
		return result, nil
	}
}

// resolveMountSource resolves a mount source path to an absolute path.
// Relative paths are resolved against repoRoot. Returns an error if the
// resolved path does not exist or is not a directory.
func resolveMountSource(source, repoRoot string) (string, error) {
	if !filepath.IsAbs(source) {
		source = filepath.Join(repoRoot, source)
	}
	abs, err := filepath.EvalSymlinks(source)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path does not exist: %s", source)
		}
		return "", fmt.Errorf("cannot resolve path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", abs)
	}
	return abs, nil
}
