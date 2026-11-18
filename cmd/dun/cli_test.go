package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The command line's three shapes (USER 2026-11-17): bare words are
// commands, -p is the headless task, --engine the JSON protocol.
func TestHeadlessTask(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		headless, eng  bool
		stdin          string
		tty, dund      bool
		task           string
		answer, engine bool
		err            string
	}{
		{name: "nothing: the TUI", engine: false},
		{name: "bare word", args: []string{"fix", "it"}, err: `unknown command "fix" — for a headless task: dun -p "fix it"`},
		{name: "subcommand after a flag", args: []string{"doctor"}, err: `"doctor" is a command: give it first`},
		{name: "-p task", args: []string{"fix", "it"}, headless: true, task: "fix it", answer: true},
		{name: "-p from stdin", headless: true, stdin: "  fix it\n", task: "fix it", answer: true},
		{name: "-p empty stdin", headless: true, err: "no task from stdin"},
		{name: "-p at a terminal", headless: true, tty: true, err: `-p needs a task`},
		{name: "-p from an older dund", headless: true, dund: true, engine: true},
		{name: "--engine", eng: true, engine: true},
		{name: "--engine with a first task", args: []string{"go"}, eng: true, task: "go", engine: true},
		{name: "both", headless: true, eng: true, err: "two modes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng := c.eng
			task, answer, err := headlessTask(c.args, c.headless, &eng, strings.NewReader(c.stdin), c.tty, c.dund)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if task != c.task || answer != c.answer || eng != c.engine {
				t.Errorf("got task %q answer %v engine %v, want %q %v %v", task, answer, eng, c.task, c.answer, c.engine)
			}
		})
	}
}

var (
	cliBinOnce sync.Once
	cliBin     string
	cliBinErr  string
)

// testDun builds this dun once per test run, under the test home TestMain
// removes.
func testDun(t *testing.T) string {
	t.Helper()
	cliBinOnce.Do(func() {
		cliBin = filepath.Join(os.Getenv("DUN_HOME"), "bin", "dun")
		build := exec.Command("go", "build", "-o", cliBin, ".")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			cliBinErr = err.Error() + "\n" + string(out)
		}
	})
	if cliBinErr != "" {
		t.Fatalf("go build: %s", cliBinErr)
	}
	return cliBin
}

// runDun runs the built dun in a temp workspace with no provider, stdin from
// the given text; it returns stdout, stderr and the exit code.
func runDun(t *testing.T, stdin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(testDun(t), args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "DUN_NO_SELFUPDATE=1", "DUN_URL=", "DUN_MODEL=", "DUN_LLM_KEY=", "DUN_DUND=")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	_ = cmd.Run()
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func TestCLI_BareWordIsRefused(t *testing.T) {
	_, stderr, code := runDun(t, "", nil, "fix it")
	if code != 2 || !strings.Contains(stderr, `unknown command "fix it" — for a headless task: dun -p "fix it"`) {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// -p is the headless run: with no provider it stops with the headless
// message (nologin.go), and prints no JSON.
func TestCLI_PIsHeadless(t *testing.T) {
	for _, args := range [][]string{{"--no-tools", "-p", "fix it"}, {"--no-tools", "-p"}} {
		stdout, stderr, code := runDun(t, "fix it from stdin\n", nil, args...)
		if code != 2 || !strings.Contains(stderr, "so the task cannot run") || strings.Contains(stdout, `"type"`) {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
	}
	if _, stderr, code := runDun(t, "", nil, "-p"); code != 2 || !strings.Contains(stderr, "no task from stdin") {
		t.Errorf("-p with empty stdin: exit %d: %s", code, stderr)
	}
}

// --engine speaks JSON events, and so does `-p` with no task from a dund
// that predates --engine (DUN_DUND set).
func TestCLI_EngineEmitsJSON(t *testing.T) {
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--engine", "--no-tools"}},
		{[]string{"DUN_DUND=1"}, []string{"-p", "--no-tools"}},
	} {
		stdout, stderr, _ := runDun(t, `{"type":"stop"}`+"\n", c.env, c.args...)
		sawSession := false
		sc := bufio.NewScanner(strings.NewReader(stdout))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var ev map[string]any
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("%v: a stdout line is not JSON: %q", c.args, sc.Text())
			}
			if ev["type"] == "session" {
				sawSession = true
			}
		}
		if !sawSession {
			t.Errorf("%v: no session event\nstdout: %s\nstderr: %s", c.args, stdout, stderr)
		}
	}
}
