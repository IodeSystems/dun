package dun

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// Session persistence layout — à la ~/.claude, scoped by the workspace ROOT.
//
//	~/.dun/sessions/<encoded-root>/<id>.jsonl
//
// The scope is the ORIGINAL workspace directory (the repo you ran dun on), NOT
// the ephemeral per-run worktree — so `dun --continue` in a repo resumes that
// repo's last conversation. $DUN_HOME overrides ~/.dun.

// SessionsDir is the root of dun's session storage (~/.dun/sessions).
func SessionsDir() string {
	if h := os.Getenv("DUN_HOME"); h != "" {
		return filepath.Join(h, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".dun", "sessions")
	}
	return filepath.Join(home, ".dun", "sessions")
}

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
func NewSessionFile(root string) (path, id string) {
	id = time.Now().Format("20060102-150405")
	return filepath.Join(RootDir(root), id+".jsonl"), id
}

// SessionFile is the path for a specific session id under root.
func SessionFile(root, id string) string {
	return filepath.Join(RootDir(root), id+".jsonl")
}

// LatestSession returns the most recent session id for root, or "" if none.
func LatestSession(root string) string {
	entries, err := os.ReadDir(RootDir(root))
	if err != nil {
		return ""
	}
	var ids []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".jsonl") {
			ids = append(ids, strings.TrimSuffix(n, ".jsonl"))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids) // ids are timestamps → lexical sort = chronological
	return ids[len(ids)-1]
}

// ListSessions returns the session ids for root, newest first.
func ListSessions(root string) []string {
	entries, _ := os.ReadDir(RootDir(root))
	var ids []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".jsonl") {
			ids = append(ids, strings.TrimSuffix(n, ".jsonl"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
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
}

// previewScanLimit bounds the work a picker does. Sessions are append-only
// JSONL and can reach megabytes; the opening ask is in the first few entries,
// so scanning past this only refines the entry COUNT, and an exact count is
// not worth making the picker wait.
const previewScanLimit = 4000

// ListSessionInfo summarizes every saved session for a workspace root, newest
// first. Unreadable sessions are listed with what is known rather than hidden —
// a session you cannot open is exactly the one you want to see.
func ListSessionInfo(root string) []SessionInfo {
	ids := ListSessions(root)
	out := make([]SessionInfo, 0, len(ids))
	for _, id := range ids {
		path := SessionFile(root, id)
		info := SessionInfo{ID: id, Path: path}
		if st, err := os.Stat(path); err == nil {
			info.ModTime = st.ModTime()
		}
		info.Entries, info.Preview = scanSession(path)
		out = append(out, info)
	}
	return out
}

// scanSession counts entries and lifts the first user message.
func scanSession(path string) (entries int, preview string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
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
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != string(agent.KindUser) {
			continue
		}
		preview = strings.Join(strings.Fields(e.Content), " ")
	}
	return entries, preview
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

// SaveSessionMeta writes the worktree metadata for a session.
func SaveSessionMeta(root, id string, meta SessionMeta) error {
	dir := RootDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := MetaFile(root, id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
