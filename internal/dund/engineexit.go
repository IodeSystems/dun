package dund

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
)

// engineExitKeep is how much of an exited engine's output is kept: a Go
// panic or fatal error with its goroutine stacks fits, a long run's log
// does not need to.
const engineExitKeep = 64 << 10

// engineLogger is an Engine whose output goes to a file (linkEngine).
type engineLogger interface{ EngineLog() string }

// keepEngineExit copies the end of an engine's output, after it exited with
// an error, beside the session's transcript (<session>.engine-exit.log). It
// returns that path and the line that says why (exitLine); "" when the
// engine has no output file or nothing was copied. The engine's own log
// (<EnginesDir>/<pid>.log) is pruned a week after; this copy stays with the
// session, and the next exit replaces it. A startup error (a bad flag, a
// missing provider) is in it too: the engine's stderr is that file from
// its first byte.
func keepEngineExit(eng Engine, workspace, id string) (path, why string) {
	l, ok := eng.(engineLogger)
	if !ok || l.EngineLog() == "" {
		return "", ""
	}
	f, err := os.Open(l.EngineLog())
	if err != nil {
		return "", ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return "", ""
	}
	if st.Size() > engineExitKeep {
		if _, err := f.Seek(st.Size()-engineExitKeep, io.SeekStart); err != nil {
			return "", ""
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, engineExitKeep))
	if err != nil || len(b) == 0 {
		return "", ""
	}
	dest := strings.TrimSuffix(harness.SessionFile(workspace, id), ".jsonl") + ".engine-exit.log"
	if err := os.WriteFile(dest, b, 0o600); err != nil {
		dest = l.EngineLog() // at least name where it is
	}
	return dest, exitLine(b)
}

// exitLine is the line of an exited engine's output that says why: a Go
// panic's or fatal error's first line, else the last line (where a startup
// error such as "dun: flag provided but not defined: -x" is), clipped.
func exitLine(b []byte) string {
	lines := strings.Split(string(b), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "panic: ") || strings.HasPrefix(l, "fatal error: ") {
			return clipLine(l, 200)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return clipLine(l, 200)
		}
	}
	return ""
}

// prefixWriter writes each line to w with a timestamp and prefix, for a
// child whose stderr shares dund's. A partial line waits for its end.
type prefixWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
	now    func() time.Time // nil: time.Now
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		// A lost log line must not stop the engine: write errors are dropped.
		_, _ = io.WriteString(p.w, now().Format("2006/01/02 15:04:05 ")+p.prefix+string(p.buf[:i+1]))
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > 64<<10 { // a line that never ends: write what there is
		_, _ = io.WriteString(p.w, now().Format("2006/01/02 15:04:05 ")+p.prefix+string(p.buf)+"\n")
		p.buf = p.buf[:0]
	}
	return len(b), nil
}
