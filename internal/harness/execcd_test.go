package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// The description names the dir each backend starts in, says a cd does not
// carry over, and claims a lasting export only for the persistent shell.
func TestExecToolDef_NamesStartDir(t *testing.T) {
	for _, tc := range []struct {
		b      ExecBackend
		dir    string
		export bool
	}{
		{&HostShell{Dir: "/src/proj"}, "/src/proj", true},
		{HostExec{Dir: "/src/proj"}, "/src/proj", false},
		{DockerExec{Dir: "/src/proj"}, "/work", false},
		{DockerExec{Dir: "/src/wt", Sub: "pkg/a"}, "/work/pkg/a", false},
		{nil, "the project root", true},
	} {
		d := execToolDef(true, tc.b).Function.Description
		if !strings.Contains(d, "Each call starts in "+tc.dir+". A `cd` does not carry over to the next call") {
			t.Errorf("%T: no start dir %q: %s", tc.b, tc.dir, d)
		}
		if strings.Contains(d, "never begin") {
			t.Errorf("%T: the cd rule is the system prompt's, not the description's: %s", tc.b, d)
		}
		if !strings.Contains(d, "`cd sub && …`") {
			t.Errorf("%T: no subdirectory form: %s", tc.b, d)
		}
		if got := strings.Contains(d, "`export` persists"); got != tc.export {
			t.Errorf("%T: export claim = %v, want %v", tc.b, got, tc.export)
		}
	}
}

func TestRedundantCd(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]bool{
		"cd " + dir + " && go test ./...":    true,
		"cd " + dir + "/ && ls":              true,
		"cd '" + dir + "'; ls":               true,
		`cd "` + dir + `" && ls`:             true,
		"  cd " + dir + "&&ls":               true,
		"cd " + link + " && ls":              true,
		"cd . && ls":                         true,
		"cd sub && ls":                       false,
		"cd " + dir + "/sub && ls":           false,
		"cd /elsewhere && ls":                false,
		"cd " + filepath.Dir(dir) + " && ls": false,
		"cd $X && ls":                        false,
		"cd " + dir:                          false, // no command after it
		"ls && cd " + dir + " && ls":         false,
	} {
		if got := redundantCd(cmd, dir); got != want {
			t.Errorf("redundantCd(%q) = %v, want %v", cmd, got, want)
		}
	}
	home, _ := os.UserHomeDir()
	for _, cmd := range []string{"cd ~ && ls", "cd $HOME/ && ls", "cd ${HOME} ; ls", "cd ~/\nls"} {
		if !redundantCd(cmd, home) {
			t.Errorf("redundantCd(%q, home) = false", cmd)
		}
	}
	if redundantCd("cd ~ && ls", dir) {
		t.Error("~ is not dir")
	}
	if redundantCd("cd x; ls", dir) || redundantCd("cd /work && ls", "/work") != true {
		t.Error("a container dir must match without existing here")
	}
}

// The line rides every redundant cd's result; a subdir or an outside path
// gets none, and the command runs as written.
func TestWithCdNudge(t *testing.T) {
	dir := t.TempDir()
	var ran []string
	inner := func(_ context.Context, tc llm.ToolCall) (string, error) {
		var a struct{ Command string }
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
		ran = append(ran, a.Command)
		return "ok", nil
	}
	d := withCdNudge(inner, dir)
	call := func(cmd string) string {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = "exec", string(args)
		out, err := d(context.Background(), tc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	line := "ok\nEach exec starts in the project dir " + dir + "; cd'ing into " + dir +
		" wastes tokens. Only cd if the directory you want is not the project dir."

	for _, cmd := range []string{"cd sub && ls", "cd /tmp && ls", "ls"} {
		if out := call(cmd); out != "ok" {
			t.Errorf("%q: %q, want no line", cmd, out)
		}
	}
	for _, redundant := range []string{"cd " + dir + " && ls", "cd " + dir + "; ls", "cd " + dir + "\nls"} {
		for i := 1; i <= 3; i++ {
			if out := call(redundant); out != line {
				t.Errorf("%q #%d: %q, want %q", redundant, i, out, line)
			}
			if ran[len(ran)-1] != redundant {
				t.Errorf("command changed: %q", ran[len(ran)-1])
			}
		}
	}
}

// The system prompt says the rule once, with the start dir; without exec
// it is absent.
func TestSystemPrompt_ExecDirRule(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "eval", ServerID: mcpserver.ServerShell}}
	want := "Each exec starts in the project dir /src/proj; never begin a command with `cd /src/proj`. " +
		"Only cd if the directory you want is not the project dir."
	s := systemFor(tools, &HostShell{Dir: "/src/proj"}, nil, "", false)
	if strings.Count(s, want) != 1 {
		t.Errorf("want the rule once: %s", s)
	}
	if strings.Contains(systemFor(tools, nil, nil, "", false), "Each exec starts") {
		t.Error("no exec: no rule")
	}
}
