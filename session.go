package dun

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
