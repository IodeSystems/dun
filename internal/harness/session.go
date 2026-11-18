package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/dunhome"
)

// Session persistence layout — à la ~/.claude, scoped by the workspace ROOT.
//
//	~/.dun/sessions/<encoded-root>/<id>.jsonl
//
// The scope is the ORIGINAL workspace directory (the repo you ran dun on), NOT
// the ephemeral per-run worktree — so `dun --continue` in a repo resumes that
// repo's last conversation. $DUN_HOME overrides ~/.dun.

// SessionsDir is the root of dun's session storage (~/.dun/sessions).
func SessionsDir() string { return filepath.Join(dunhome.Dir(), "sessions") }

// RootDir is the sessions directory for one workspace root — the abs path
// flattened to a single, filesystem-safe name (leading marker kept for read-back).
func RootDir(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	name := strings.NewReplacer(string(os.PathSeparator), "-", " ", "_").Replace(abs)
	return filepath.Join(SessionsDir(), name)
}

// NewSessionFile returns the path for a fresh session under root, plus its id.
//
// The id is a timestamp to the second, and two dun processes started in one
// workspace within a second used to get the SAME file: the second loaded the
// first's history and rewrote the file under it (found in M0, 2026-10-20, when
// back-to-back probe sessions 0.6s apart shared one transcript). So the file
// is claimed with O_EXCL, and a taken id gets a "-2", "-3"… suffix, which still
// sorts after the bare id and before the next second's.
func NewSessionFile(root string) (path, id string) {
	base := time.Now().Format("20060102-150405")
	dir := RootDir(root)
	_ = os.MkdirAll(dir, 0o700)
	for n := 1; n <= 99; n++ {
		id = base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		path = filepath.Join(dir, id+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return path, id
		}
		if !os.IsExist(err) {
			break // not a collision (permissions, disk): let the store report it
		}
	}
	return filepath.Join(dir, base+".jsonl"), base
}

// sessionIDs lists the session ids under root, unsorted. A session id never
// contains a dot: "<id>.recapN.jsonl", "<id>.reheadN.jsonl" and
// "<id>.subN.jsonl" are a session's sidecars and children, not sessions — a
// lexical "latest" over all *.jsonl picked "<id>.rehead1" over "<id>" itself.
//
// An EMPTY session file is skipped too: NewSessionFile claims the file at
// start, so a dun that exits before saying anything leaves one, and
// --continue must not resume a conversation that never happened. Those are
// removed by PruneSessions (emptySessionIDs).
func sessionIDs(root string) []string {
	ids, _ := scanSessionIDs(root)
	return ids
}

// emptySessionIDs lists claimed-but-empty sessions last modified before
// cutoff — old enough that no live process is about to write to them.
func emptySessionIDs(root string, cutoff time.Time) []string {
	_, empty := scanSessionIDs(root)
	var out []string
	for id, mod := range empty {
		if mod.Before(cutoff) {
			out = append(out, id)
		}
	}
	return out
}

func scanSessionIDs(root string) (ids []string, empty map[string]time.Time) {
	entries, _ := os.ReadDir(RootDir(root))
	empty = map[string]time.Time{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(n, ".jsonl")
		if strings.Contains(id, ".") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() == 0 {
			empty[id] = info.ModTime()
			continue
		}
		ids = append(ids, id)
	}
	return ids, empty
}

// SessionAsked reports whether anything was asked in a session: a user
// message in its transcript or in an archive of it — the transcript /clear set
// aside ("<id>.cleared.<time>.jsonl") or a recap's ("<id>.recapN.jsonl").
// It is THE rule for "this session was used": PruneSessions and dund both ask
// it, so what one keeps the other keeps too. /clear leaves a transcript of
// notices only, and judging by that alone deleted the archive with the
// session (2026-11-14).
//
// A file that cannot be read counts as asked: it is exactly the one to look
// at. A transcript that does not exist does not. It never creates anything.
func SessionAsked(root, id string) bool {
	files := []string{SessionFile(root, id)}
	for _, pat := range []string{".cleared.*.jsonl", ".recap*.jsonl"} {
		m, _ := filepath.Glob(filepath.Join(RootDir(root), id+pat))
		files = append(files, m...)
	}
	for _, f := range files {
		if fileAsked(f) {
			return true
		}
	}
	return false
}

// fileAsked reports whether a transcript file holds a user message (typed, or
// delivered mid-turn), or cannot be read. It reads the whole file: one user
// message after any number of notices still counts.
func fileAsked(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return !os.IsNotExist(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var e struct {
			Kind string `json:"Kind"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			return true
		}
		if e.Kind == string(agent.KindUser) || e.Kind == string(kindDelivered) {
			return true
		}
	}
	return sc.Err() != nil
}

// SessionFile is the path for a specific session id under root.
func SessionFile(root, id string) string {
	return filepath.Join(RootDir(root), id+".jsonl")
}

// LatestSession returns the most recent session id for root, or "" if none.
func LatestSession(root string) string {
	ids := sessionIDs(root)
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids) // ids are timestamps → lexical sort = chronological
	return ids[len(ids)-1]
}

// ListSessions returns the session ids for root, newest first.
func ListSessions(root string) []string {
	ids := sessionIDs(root)
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// ListAskedSessions returns the ids of sessions that were asked in
// (SessionAsked), newest first, at most limit of them (limit <= 0: all). It
// includes a session whose live transcript is empty or notices-only after a
// /clear: the archive holds the ask. Every listing goes through it.
func ListAskedSessions(root string, limit int) []string {
	ids, empty := scanSessionIDs(root)
	for id := range empty {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		if SessionAsked(root, id) {
			out = append(out, id)
		}
	}
	return out
}

// SessionInfo is one saved session, summarized for a picker.
//
// The id alone is a timestamp, which tells you when but never WHICH — and a
// workspace accumulates dozens. Preview is the opening ask, because that is
// how a person actually recognises a conversation ("the one where I asked about
// compaction"), and Entries separates a real session from the two-line one you
// abandoned.
type SessionInfo struct {
	ID      string
	Path    string
	ModTime time.Time
	Entries int
	Preview string
	// Last is the latest user message, when it is not the opening one: what
	// the session was about by the end (USER 2026-10-29). "" when the end of
	// the file holds none.
	Last string
}

// previewScanLimit bounds the work a picker does. Sessions are append-only
// JSONL and can reach megabytes; the opening ask is in the first few entries,
// so scanning past this only refines the entry COUNT, and an exact count is
// not worth making the picker wait.
const previewScanLimit = 4000

// ListSessionInfo summarizes every saved session with a user message in it for
// a workspace root, newest first. Unreadable sessions are listed with what is
// known rather than hidden — a session you cannot open is exactly the one you
// want to see.
func ListSessionInfo(root string) []SessionInfo {
	ids := ListAskedSessions(root, 0)
	out := make([]SessionInfo, 0, len(ids))
	for _, id := range ids {
		path := SessionFile(root, id)
		info := SessionInfo{ID: id, Path: path}
		if st, err := os.Stat(path); err == nil {
			info.ModTime = st.ModTime()
		}
		info.Entries, info.Preview, _ = scanSession(path)
		if last := lastUserMessage(path); last != info.Preview {
			info.Last = last
		}
		out = append(out, info)
	}
	return out
}

// scanSession counts entries and lifts the first user message; unreadable is
// a file that would not open or a line that is not an entry.
func scanSession(path string) (entries int, preview string, unreadable bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() && entries < previewScanLimit {
		entries++
		if preview != "" {
			continue
		}
		var e struct {
			Kind    string `json:"Kind"`
			Content string `json:"Content"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			unreadable = true
			continue
		}
		if e.Kind != string(agent.KindUser) {
			continue
		}
		preview = strings.Join(strings.Fields(e.Content), " ")
	}
	return entries, preview, unreadable || sc.Err() != nil
}

// lastTailBytes is how much of a session's end lastUserMessage reads: a
// session can be megabytes, and its last ask is near the end.
const lastTailBytes = 512 << 10

// lastUserMessage is the latest user message in the last lastTailBytes of
// a session file; "" when there is none there.
func lastUserMessage(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(0, st.Size()-lastTailBytes)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(string(buf), "\n")
	if off > 0 {
		lines = lines[1:] // starts mid-line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Kind    string `json:"Kind"`
			Content string `json:"Content"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil || e.Kind != string(agent.KindUser) {
			continue
		}
		if c := strings.Join(strings.Fields(e.Content), " "); c != "" {
			return c
		}
	}
	return ""
}

// SessionMeta stores the worktree info alongside a session so that resume
// can reuse the same worktree (preserving file edits).
type SessionMeta struct {
	WorktreePath string `json:"worktreePath,omitempty"`
	Branch       string `json:"branch,omitempty"`
}

// MetaFile returns the path for the .meta.json sidecar of a session.
func MetaFile(root, id string) string {
	return filepath.Join(RootDir(root), id+".meta.json")
}

// StatsFile is the session's .stats.json sidecar: the totals the next engine
// on the session seeds from (cmd/dun/tally.go).
func StatsFile(root, id string) string {
	return filepath.Join(RootDir(root), id+".stats.json")
}

// SaveSessionMeta writes the worktree metadata for a session.
func SaveSessionMeta(root, id string, meta SessionMeta) error {
	dir := RootDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := MetaFile(root, id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, MetaFile(root, id))
}

// LoadSessionMeta reads the worktree metadata for a session, or zero if none.
func LoadSessionMeta(root, id string) SessionMeta {
	data, err := os.ReadFile(MetaFile(root, id))
	if err != nil {
		return SessionMeta{}
	}
	var meta SessionMeta
	if json.Unmarshal(data, &meta) != nil {
		return SessionMeta{}
	}
	return meta
}

// DunHome is $DUN_HOME, else ~/.dun.
func DunHome() string { return filepath.Dir(SessionsDir()) }
