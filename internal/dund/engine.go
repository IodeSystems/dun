package dund

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Engine is one running `dun -p`: commands go in on Stdin, events come out on
// Stdout, one NDJSON frame per line.
type Engine interface {
	Stdin() io.Writer
	Stdout() io.Reader
	// Wait blocks until the process is gone.
	Wait() error
	// Kill ends it now.
	Kill()
}

// Spawner starts engines. The real one execs the dun binary; tests start
// fakes.
type Spawner interface {
	Spawn(workspace string, args []string) (Engine, error)
}

// ExecSpawner runs `<Bin> -p --workspace <ws> <args…>`.
type ExecSpawner struct {
	Bin string
	Env []string // extra environment, beside the service's own
}

type execEngine struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
}

func (s ExecSpawner) Spawn(workspace string, args []string) (Engine, error) {
	cmd := exec.Command(s.Bin, append([]string{"-p", "--workspace", workspace}, args...)...)
	// A spawned engine never rebuilds itself; the service owns freshness.
	// DUN_DUND tells it dund is on the other end of its stdio, so it offers
	// dun_list and dun_msg (proto/peer.go).
	cmd.Env = append(append(os.Environ(), "DUN_CHILD=1", "DUN_DUND=1"), s.Env...)
	cmd.Stderr = os.Stderr
	ownGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", s.Bin, err)
	}
	return &execEngine{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (e *execEngine) Stdin() io.Writer  { return e.stdin }
func (e *execEngine) Stdout() io.Reader { return e.stdout }
func (e *execEngine) Wait() error       { return e.cmd.Wait() }
func (e *execEngine) Kill() {
	_ = e.stdin.Close()
	killGroup(e.cmd)
}

// allowedFlags are the engine flags a client may pass to create. --key is
// not among them: the engine reads the saved key, and a key in a command
// would travel through clients and logs. --resume and --continue are the
// service's to set.
var allowedFlags = map[string]bool{
	"--model": true, "--url": true, "--worktree": true, "--docker": true,
	"--docker-network": true, "--pr": true, "--ship": true, "--no-ship": true, "--rag": true,
	"--lsp": true, "--rephrase": true, "--no-suggest": true, "--no-dir": true,
}

// takesValue are the allowed flags whose value may be the next argument.
var takesValue = map[string]bool{"--model": true, "--url": true, "--docker": true}

func checkArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		name, _, hasEq := strings.Cut(args[i], "=")
		if !allowedFlags[name] {
			return fmt.Errorf("engine flag %q is not allowed", args[i])
		}
		if takesValue[name] && !hasEq {
			if i+1 >= len(args) {
				return fmt.Errorf("engine flag %s needs a value", name)
			}
			i++
		}
	}
	return nil
}
