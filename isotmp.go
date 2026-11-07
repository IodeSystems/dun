package dun

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The isolation tmp: where saved output (a clipped exec result, a background
// job's log) can be READ, at one absolute path that means the same file on
// the host, in a worktree and inside a --docker container (mounted there at
// the same path). file_read already reads absolute paths under /tmp, and any
// exec can cat or grep it.
//
// Saved output is written twice (USER 2026-11-06, "dual write and isolation
// restore"): the session's bg dir is the durable copy that travels with the
// session, the isolation tmp is the read surface. /tmp does not survive a
// reboot, so restoreIsoTmp copies back what is missing when the isolation
// comes up (start, resume, rehoist). The dir is never removed by dun: a
// server's log outlives the session that started the server.
//
// A child shares its parent's dir under child-N, so a parent can read a
// child's job log by path.

var subSession = regexp.MustCompile(`^(.*)\.sub([0-9]+)$`)

// IsoTmpDir is the isolation tmp for a session file, "" for an in-memory
// session (its bg dir is already under the temp dir).
func IsoTmpDir(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	id := strings.TrimSuffix(filepath.Base(sessionFile), ".jsonl")
	if m := subSession.FindStringSubmatch(id); m != nil {
		return filepath.Join(os.TempDir(), "dun-"+m[1], "child-"+m[2])
	}
	return filepath.Join(os.TempDir(), "dun-"+id)
}

// MkIsoTmp is IsoTmpDir, created: a docker volume source that does not exist
// is made by the daemon, owned by root. "" when there is none or it cannot be
// made (saved output then stays in the bg dir alone).
func MkIsoTmp(sessionFile string) string {
	dir := IsoTmpDir(sessionFile)
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	return dir
}

// isoTmp is this session's isolation tmp, created.
func (h *Harness) isoTmp() string { return MkIsoTmp(h.cfg.SessionFile) }

// saveBoth writes data as name in the bg dir and in the isolation tmp, and
// returns the path the model is given: the tmp copy, or the bg one when there
// is no tmp. "" when neither could be written.
func (h *Harness) saveBoth(name string, data []byte) string {
	durable := ""
	if dir := h.bgLogDir(); dir != "" {
		p := filepath.Join(dir, name)
		if os.WriteFile(p, data, 0o600) == nil {
			durable = p
		}
	}
	if tmp := h.isoTmp(); tmp != "" && tmp != h.bgLogDir() {
		p := filepath.Join(tmp, name)
		if os.WriteFile(p, data, 0o600) == nil {
			return p
		}
	}
	return durable
}

// dualLog is a job log open in both places. Writes go to both; a copy that
// fails is dropped rather than failing the job.
type dualLog struct{ files []*os.File }

func (d *dualLog) Write(p []byte) (int, error) {
	for _, f := range d.files {
		_, _ = f.Write(p)
	}
	return len(p), nil
}

func (d *dualLog) Close() error {
	for _, f := range d.files {
		_ = f.Close()
	}
	return nil
}

// createBoth opens name for writing in the bg dir and the isolation tmp. The
// path returned is the one the model is given (as saveBoth).
func (h *Harness) createBoth(name string) (io.WriteCloser, string) {
	d := &dualLog{}
	path := ""
	if dir := h.bgLogDir(); dir != "" {
		if f, err := os.Create(filepath.Join(dir, name)); err == nil {
			d.files, path = append(d.files, f), f.Name()
		}
	}
	if tmp := h.isoTmp(); tmp != "" && tmp != h.bgLogDir() {
		if f, err := os.Create(filepath.Join(tmp, name)); err == nil {
			d.files, path = append(d.files, f), f.Name()
		}
	}
	if len(d.files) == 0 {
		return nil, ""
	}
	return d, path
}

// restoreIsoTmp copies into the isolation tmp every saved file it lacks: after
// a reboot wiped /tmp, or on a resume or rehoist. A file already there is
// left alone (a running job's log is being written to it).
func (h *Harness) restoreIsoTmp() {
	tmp, src := h.isoTmp(), h.bgLogDir()
	if tmp == "" || src == "" || tmp == src {
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		dst := filepath.Join(tmp, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		_ = copyFile(filepath.Join(src, e.Name()), dst, 0o600)
	}
}
