package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureDunImage checks if the "dun:local" Docker image exists and builds it
// if not. It finds the Dockerfile next to the dun binary (or in the repo root)
// and copies the required binaries into a build context.
func ensureDunImage() error {
	// Check if the image already exists
	cmd := exec.Command("docker", "inspect", "dun:local")
	if err := cmd.Run(); err != nil {
		// Image doesn't exist — build it
		return buildDunImage()
	}
	return nil
}

func buildDunImage() error {
	fmt.Fprintln(os.Stderr, "dun: building docker image dun:local ...")

	// Find the Dockerfile — look next to the binary, then in common locations.
	selfPath, err := os.Executable()
	if err != nil {
		selfPath = "dun"
	}
	selfDir := filepath.Dir(selfPath)

	// Find the repo root: walk up from self looking for Dockerfile
	dockerfile := findDockerfile(selfDir)
	if dockerfile == "" {
		// Fallback: try $HOME/go/bin parent trees, then cwd
		if cwd, _ := os.Getwd(); cwd != "" {
			dockerfile = findDockerfile(cwd)
		}
	}
	if dockerfile == "" {
		return fmt.Errorf("dockerfile not found — place one next to the dun binary or run tools/build-docker.sh")
	}

	// Find the required binaries on PATH
	bins := []string{"dun", "poly-lsp-mcp", "mcpshell", "raglit"}
	binPaths := make(map[string]string)
	for _, bin := range bins {
		p, err := exec.LookPath(bin)
		if err != nil {
			return fmt.Errorf("%s not found on PATH", bin)
		}
		binPaths[bin] = p
	}

	// Create a temp build context
	tmpdir, err := os.MkdirTemp("", "dun-docker-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	// Copy binaries
	for name, path := range binPaths {
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.WriteFile(filepath.Join(tmpdir, name), src, 0755); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	// Copy Dockerfile
	dockerfileContent, err := os.ReadFile(dockerfile)
	if err != nil {
		return fmt.Errorf("read Dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "Dockerfile"), dockerfileContent, 0644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}

	// Build
	cmd := exec.Command("docker", "build", "-t", "dun:local", tmpdir)
	cmd.Stdout = os.Stderr // build output goes to stderr (stdout is for -p events)
	cmd.Stderr = os.Stderr
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.WriteFile(filepath.Join(tmpdir, "build.log"), out, 0644)
		return fmt.Errorf("docker build failed: %w", err)
	}

	fmt.Fprintln(os.Stderr, "dun: docker image dun:local built")
	return nil
}

// findDockerfile walks up from dir looking for a Dockerfile.
func findDockerfile(start string) string {
	for dir := start; ; {
		candidate := filepath.Join(dir, "Dockerfile")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// dockerReexecArgs builds the argument list for re-executing dun inside a
// Docker container, stripping docker-specific flags and remapping --workspace.
func dockerReexecArgs() []string {
	reexec := make([]string, 0, len(os.Args))
	skipNext := false
	for _, a := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--docker" || strings.HasPrefix(a, "--docker=") || strings.HasPrefix(a, "--docker-network") {
			continue
		}
		if strings.HasPrefix(a, "--workspace=") {
			reexec = append(reexec, "--workspace=/work")
		} else if a == "--workspace" {
			reexec = append(reexec, "--workspace=/work")
			skipNext = true
		} else {
			reexec = append(reexec, a)
		}
	}
	return reexec
}
