package dun

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	filetools "github.com/iodesystems/agentkit/files"
)

// Built-in file tools: agentkit/files mounted next to exec.
//
// Without them the model reads with `cat`, edits with `sed` and heredocs, and
// searches with `grep -r` through exec — unbounded output, no line numbers to
// page by, and an edit whose only receipt is an exit code. The toolset is
// rooted at the session's working tree (the worktree when one is in use), so
// a path that climbs out is refused rather than resolved somewhere nobody
// looks.
//
// Config, in any dun.json layer:
//
//	{"files": {"tools": ["read","edit","write","grep","list"],
//	           "read_only": false, "read_max": 65536, "ignore": ["gen/"]}}
//
// tools: [] turns them all off. delete and move are available but off unless
// listed.

// FilesConfig is the "files" section of dun.json.
type FilesConfig struct {
	// Tools enables tools by name: read, edit, write, grep, list, delete,
	// move. nil inherits (default: read, edit, write, grep, list); an empty
	// list turns the file tools off.
	// No omitempty: null inherits and [] is off, and both must round-trip.
	Tools []string `json:"tools"`
	// ReadOnly keeps read, grep and list only. A pointer so a higher layer can
	// say false.
	ReadOnly *bool `json:"read_only,omitempty"`
	// Caps; 0 inherits agentkit's defaults (64KB ranged read, 8KB unranged,
	// 100 grep matches, 200 list entries).
	ReadMax     int `json:"read_max,omitempty"`
	UnrangedMax int `json:"unranged_max,omitempty"`
	GrepMax     int `json:"grep_max,omitempty"`
	ListMax     int `json:"list_max,omitempty"`
	// Ignore are gitignore-syntax patterns hidden from grep and list on top
	// of .gitignore. Accumulated across layers, not replaced.
	Ignore []string `json:"ignore,omitempty"`
}

// LoadFiles resolves the effective files config for a workspace across the
// same layers as LoadShip. nil means no section anywhere: the defaults.
func LoadFiles(dir string) *FilesConfig {
	var out *FilesConfig
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(path)
		if err != nil || f == nil || f.Files == nil {
			continue
		}
		if out == nil {
			cp := *f.Files
			out = &cp
			continue
		}
		merged := mergeFiles(*out, *f.Files)
		out = &merged
	}
	return out
}

// mergeFiles layers next over prev: what next states wins, what it does not
// state is inherited, and ignore patterns add up.
func mergeFiles(prev, next FilesConfig) FilesConfig {
	if next.Tools != nil {
		prev.Tools = next.Tools
	}
	if next.ReadOnly != nil {
		prev.ReadOnly = next.ReadOnly
	}
	if next.ReadMax != 0 {
		prev.ReadMax = next.ReadMax
	}
	if next.UnrangedMax != 0 {
		prev.UnrangedMax = next.UnrangedMax
	}
	if next.GrepMax != 0 {
		prev.GrepMax = next.GrepMax
	}
	if next.ListMax != 0 {
		prev.ListMax = next.ListMax
	}
	if len(next.Ignore) > 0 {
		prev.Ignore = append(append([]string(nil), prev.Ignore...), next.Ignore...)
	}
	return prev
}

// toolsetConfig is the agentkit config for root. ok=false: file tools off.
func (c *FilesConfig) toolsetConfig(root string) (filetools.Config, bool) {
	cfg := filetools.Config{Root: root}
	if c == nil {
		return cfg, root != ""
	}
	if c.Tools != nil && len(c.Tools) == 0 {
		return cfg, false
	}
	cfg.Tools = c.Tools
	cfg.ReadOnly = c.ReadOnly != nil && *c.ReadOnly
	cfg.ReadMax, cfg.UnrangedMax = c.ReadMax, c.UnrangedMax
	cfg.GrepMax, cfg.ListMax = c.GrepMax, c.ListMax
	cfg.ExtraIgnore = c.Ignore
	return cfg, root != ""
}

// fileTools returns the session's toolset for its current working tree,
// building it on first use and again whenever the root or the config moves.
// Kept across rebuilds otherwise, because the toolset remembers which files
// this session has read — what write's overwrite guard consults — and a
// server starting is no reason to forget that. Callers hold turnMu.
func (h *Harness) fileTools() *filetools.Toolset {
	cfg, ok := h.cfg.Files.toolsetConfig(h.cfg.Workspace)
	if !ok {
		h.files, h.filesKey = nil, ""
		return nil
	}
	key, _ := json.Marshal(struct {
		Root string
		Cfg  *FilesConfig
	}{h.cfg.Workspace, h.cfg.Files})
	if h.files != nil && h.filesKey == string(key) {
		return h.files
	}
	ts, err := filetools.New(cfg)
	if err != nil {
		log.Printf("dun: file tools off: %v", err)
		h.files, h.filesKey = nil, ""
		return nil
	}
	h.files, h.filesKey = ts, string(key)
	return ts
}

// tmpRoots are the directories read, grep and list may also look in by
// absolute path: scratch output, a test's log, a command's dump all land there,
// and refusing the path exec just printed sends the model back to cat (USER
// 2026-10-23). Reads only — a change to /tmp still goes through exec.
var tmpRoots = func() []string { // a var: a test whose dirs are in /tmp turns it off
	roots := []string{"/tmp"}
	if d := os.TempDir(); filepath.Clean(d) != "/tmp" {
		roots = append(roots, d)
	}
	var out []string
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// tmpReaders is a read-only toolset per tmp root, configured like ts (same
// caps and prefix). Resolve keeps the path as the model wrote it, so a read's
// header says /tmp/x.log, not x.log.
func tmpReaders(cfg filetools.Config) []*filetools.Toolset {
	var out []*filetools.Toolset
	for _, root := range tmpRoots() {
		c := cfg
		c.Root, c.ReadOnly, c.ExtraIgnore = root, true, nil
		c.Resolve = func(rel string) (string, error) { return filetools.Under(root, rel) }
		ts, err := filetools.New(c)
		if err != nil {
			continue
		}
		out = append(out, ts)
	}
	return out
}

// tmpReaders is tmpReaders for this session's files config; nil when the
// file tools are off. Rebuilt with the tool set: they track no reads that
// matter, since nothing writes through them.
func (h *Harness) tmpReaders() []*filetools.Toolset {
	cfg, ok := h.cfg.Files.toolsetConfig(h.cfg.Workspace)
	if !ok {
		return nil
	}
	return tmpReaders(cfg)
}

// tmpReader is the tmp toolset for a read, grep or list call whose path is
// absolute, outside the working tree, and under a tmp root; nil otherwise.
func tmpReader(ts *filetools.Toolset, tmp []*filetools.Toolset, name, args string) *filetools.Toolset {
	switch ts.Bare(name) {
	case filetools.Read, filetools.Grep, filetools.List:
	default:
		return nil
	}
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || !filepath.IsAbs(strings.TrimSpace(a.Path)) {
		return nil
	}
	p := filepath.Clean(strings.TrimSpace(a.Path))
	if _, err := filetools.Under(ts.Root(), p); err == nil {
		return nil // inside the working tree: the tree's toolset has it
	}
	for _, t := range tmp {
		if _, err := filetools.Under(t.Root(), p); err == nil {
			return t
		}
	}
	return nil
}

// withFiles routes the file tools to the toolset and reports each call.
func withFiles(inner agent.ToolDispatcher, ts *filetools.Toolset, tmp, added []*filetools.Toolset, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if !ts.Handles(tc.Function.Name) {
			return inner(ctx, tc)
		}
		route := ts
		out := ""
		if t := tmpReader(ts, tmp, tc.Function.Name, tc.Function.Arguments); t != nil {
			route = t
		} else if t := addedRoute(ts, added, tc.Function.Arguments); t != nil {
			route = t // a path the person added (/file add, fileadd.go)
		} else if hint := outsideHint(ts, tc.Function.Name, tc.Function.Arguments); hint != "" {
			out = hint
		}
		if out == "" {
			out = route.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
		}
		if onCall != nil {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			onCall(tc.Function.Name, args, out)
		}
		return out, nil
	}
}

// systemFiles is the system prompt's line for the file tools, naming only the
// ones this session holds.
func systemFiles(ts *filetools.Toolset) string {
	if ts == nil {
		return ""
	}
	names := ts.Names()
	if len(names) == 0 {
		return ""
	}
	line := "\n- " + strings.Join(names, ", ") + ": read, search and change files in the working tree, by paths relative to it " +
		"(read, grep and list also take an absolute path under /tmp, and all of them a path the user added with /file add). " +
		"Prefer them to exec for reading files (not cat/head/sed), searching (not grep/rg/find)"
	if ts.Handles(ts.Name(filetools.Edit)) {
		line += " and editing (not sed or heredocs)"
	}
	line += ": reads are line-numbered and paged, searches respect .gitignore, and edits return the diff. " +
		"Read before you edit; copy edit anchors from what " + ts.Name(filetools.Read) + " returned, without the line numbers."
	return line
}

// addedRoute is the added path's toolset for a call whose path is absolute
// and under one the person added; nil otherwise.
func addedRoute(ts *filetools.Toolset, added []*filetools.Toolset, args string) *filetools.Toolset {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || !filepath.IsAbs(strings.TrimSpace(a.Path)) {
		return nil
	}
	p := filepath.Clean(strings.TrimSpace(a.Path))
	if _, err := filetools.Under(ts.Root(), p); err == nil {
		return nil
	}
	for _, t := range added {
		if _, err := filetools.Under(t.Root(), p); err == nil {
			return t
		}
	}
	return nil
}
