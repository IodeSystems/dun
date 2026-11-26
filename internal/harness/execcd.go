package harness

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// execcd.go — the leading `cd <start dir> &&` (USER 2026-11-18).
//
// Every exec call starts in its backend's dir: HostShell cds back to Dir
// before each command, HostExec sets cmd.Dir, DockerExec passes -w. Models
// prefix commands with `cd <that dir> &&` all the same. Measured on this box,
// sessions of the 60 days to 2026-11-18: 751 exec calls, 497 began with
// `cd <path> &&` or `;`, and 233 of those named the session's own root.
// The exec description and the system prompt name the dir. A command that
// still begins with it gets one line in its result, every time.

// execStartDir is the directory a backend's commands start in, as the
// command sees it: the host path, or the container path under --docker. ""
// for a backend that does not say.
func execStartDir(b ExecBackend) string {
	switch e := b.(type) {
	case *HostShell:
		return e.Dir
	case HostExec:
		return e.Dir
	case DockerExec:
		return path.Join("/work", filepath.ToSlash(e.Sub))
	}
	return ""
}

// leadingCd matches a command that begins `cd <path> &&` or `cd <path>;`,
// the path bare or in one pair of quotes.
var leadingCd = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|'"]+)\s*(&&|;|\n)`)

// redundantCd says whether command begins with a cd to dir itself.
func redundantCd(command, dir string) bool {
	if dir == "" {
		return false
	}
	m := leadingCd.FindStringSubmatch(command)
	if m == nil {
		return false
	}
	p := strings.Trim(m[1], `"'`)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		p = home + p[1:]
	}
	for _, v := range []string{"${HOME}", "$HOME"} {
		if p == v || strings.HasPrefix(p, v+"/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return false
			}
			p = home + p[len(v):]
			break
		}
	}
	if strings.Contains(p, "$") {
		return false // a variable: not ours to expand
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if p == dir {
		return true
	}
	// A symlinked spelling of the same dir (host only: a container path
	// does not exist here, and EvalSymlinks fails on it).
	rp, err1 := filepath.EvalSymlinks(p)
	rd, err2 := filepath.EvalSymlinks(dir)
	return err1 == nil && err2 == nil && rp == rd
}

// withCdNudge adds a line to an exec result whose command began with a cd
// to the dir it already starts in. The command runs unchanged. The line
// rides every redundant cd: one session had 41 of them, and a line on every
// 3rd (10 lines) did not stop the cd (USER 2026-11-20).
//
// The model's view of its own earlier calls stays as it wrote them: a
// history that differs from what the model wrote is worse than the cd.
func withCdNudge(inner agent.ToolDispatcher, dir string) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		out, err := inner(ctx, tc)
		if tc.Function.Name != "exec" || err != nil {
			return out, err
		}
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if redundantCd(args.Command, dir) {
			out.Text += "\n" + cdNudge(dir)
		}
		return out, err
	}
}

// cdNudge is the line a redundant cd's result carries.
func cdNudge(dir string) string {
	return "Each exec starts in the project dir " + dir + "; cd'ing into " + dir +
		" wastes tokens. Only cd if the directory you want is not the project dir."
}
