package harness

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

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
// The exec description names the dir (execToolDef); a command that still
// begins with it gets one line in its result, the first few times.

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

// cdNudgeMax is how many results in a session carry the line: enough to be
// read, few enough not to be noise.
const cdNudgeMax = 3

// leadingCd matches a command that begins `cd <path> &&` or `cd <path>;`,
// the path bare or in one pair of quotes.
var leadingCd = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|'"]+)\s*(&&|;)`)

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
// to the dir it already starts in. The command ran unchanged. shown counts
// the lines across the session; past cdNudgeMax none is added.
func withCdNudge(inner agent.ToolDispatcher, dir string, shown *atomic.Int32) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		out, err := inner(ctx, tc)
		if tc.Function.Name != "exec" || err != nil || shown.Load() >= cdNudgeMax {
			return out, err
		}
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if redundantCd(args.Command, dir) && shown.Add(1) <= cdNudgeMax {
			out += "\n(exec already starts in " + dir + "; the cd was not needed)"
		}
		return out, err
	}
}
