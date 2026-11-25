package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"

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
//	{"files": {"tools": ["read","edit","write"],
//	           "read_only": false, "read_max": 65536, "ignore": ["gen/"]}}
//
// tools: [] turns them all off. grep, list, delete and move are available but
// off unless listed: read lists a directory and, with grep, searches a file
// or a tree, so grep and list only add definitions to every request.

// DefaultFileTools are the file tools a session gets when no config names
// any. Not agentkit's default, which also has grep and list: read does both
// (on a directory it lists; with grep it searches), and each tool's
// definition rides every request. A benchmark (2026-10-27) had the model
// read the last 120 lines of a 218KB CI log, ~5.8k tokens carried for four
// more requests, where another agent's grep of it returned 265 bytes; one
// tool that reads and searches is the one a model reaches for.
var DefaultFileTools = []string{filetools.Read, filetools.Edit, filetools.Write}

// filesToolset is the agentkit config for root. ok=false: file tools off.
func filesToolset(c *mcpserver.FilesConfig, root string) (filetools.Config, bool) {
	cfg := filetools.Config{Root: root, Tools: DefaultFileTools}
	if c == nil {
		return cfg, root != ""
	}
	if c.Tools != nil && len(c.Tools) == 0 {
		return cfg, false
	}
	if c.Tools != nil {
		cfg.Tools = c.Tools
	}
	// read_only does not reach agentkit: its ReadOnly drops edit and write
	// from the list, and a model without them edited through exec. The
	// workspace's mode (workspaceWrite, read_only included) is withFiles'
	// to refuse, with the command that changes it.
	cfg.ReadMax, cfg.UnrangedMax = c.ReadMax, c.UnrangedMax
	cfg.GrepMax, cfg.ListMax = c.GrepMax, c.ListMax
	cfg.ExtraIgnore = append([]string(nil), c.Ignore...)
	// A session that works in the main checkout pays a full-tree grep for
	// every other agent's worktree copy of the same files (a planning
	// session, 2026-11-20: 116s in .claude/worktrees for noise it had
	// already located on base). The copies are the RAG ingest's
	// workspaceCopies; skip them here too. A session in its own worktree
	// is left as configured: it may name a sibling worktree deliberately.
	if workspaceIsMainCheckout(root) {
		for d := range workspaceCopies {
			cfg.ExtraIgnore = append(cfg.ExtraIgnore, d+"/")
		}
	}
	return cfg, root != ""
}

// workspaceIsMainCheckout reports whether root is the main checkout of the
// git repo containing it (the --no-worktree, work-in-place case): not a
// repo, and not a linked worktree of a repo checked out elsewhere.
func workspaceIsMainCheckout(root string) bool {
	if root == "" {
		return false
	}
	top, repo := RepoRoot(root), MainRepo(root)
	return top != "" && sameDir(top, repo)
}

// fileTools returns the session's toolset for its current working tree,
// building it on first use and again whenever the root or the config moves.
// Kept across rebuilds otherwise, because the toolset remembers which files
// this session has read — what write's overwrite guard consults — and a
// server starting is no reason to forget that. Callers hold turnMu.
func (h *Harness) fileTools() *filetools.Toolset {
	cfg, ok := filesToolset(h.cfg.Files, h.cfg.Workspace)
	if !ok {
		h.files, h.filesKey = nil, ""
		return nil
	}
	key, _ := json.Marshal(struct {
		Root string
		Cfg  *mcpserver.FilesConfig
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

// tmpReaders is a read-only toolset per root (sessionReadDirs), configured
// like cfg (same caps and prefix). Resolve keeps the path as the model wrote
// it, so a read's header says /tmp/dun-<id>/job-1.log, not job-1.log. A root
// is real (symlinks resolved) and the path is resolved to match: on macOS the
// temp dir is /var/folders/…, a link to /private/var/folders/…. The whole temp
// dir is not a root (USER 2026-11-18): another path there asks, as any path
// outside the workspace does (fileaccess.go).
func tmpReaders(cfg filetools.Config, roots ...string) []*filetools.Toolset {
	var out []*filetools.Toolset
	for _, root := range roots {
		c := cfg
		c.Root, c.ReadOnly, c.ExtraIgnore = root, true, nil
		c.Resolve = func(rel string) (string, error) { return filetools.Under(root, realAbs(rel)) }
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
	cfg, ok := filesToolset(h.cfg.Files, h.cfg.Workspace)
	if !ok {
		return nil
	}
	return tmpReaders(cfg, h.sessionReadDirs()...)
}

// sessionReadDirs are the directories where dun saves files for this session
// and gives the model their paths: the isolation tmp (job logs, clipped exec
// output, harness.json; isotmp.go) and the shell's overflow dir
// (shellhost.go). Read, grep and list open them with no grant and no
// question: the model was refused the log of a job dun started for it
// (macOS, /var/folders/…/dun-<id>/job-18.log). A sub-agent gets its
// ancestors' dirs too, so it can read a parent's job log by the path it was
// passed; a parent's isolation tmp already holds its children's (child-N).
// Exact dirs, real paths: not another session's, not the whole temp dir.
func (h *Harness) sessionReadDirs() []string {
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		if real, err := filepath.EvalSymlinks(d); err == nil && !slices.Contains(out, real) {
			out = append(out, real)
		}
	}
	if tmp := h.isoTmp(); tmp != "" {
		add(tmp)
	} else {
		add(h.bgLogDir()) // saveBoth's path then: no isolation tmp
	}
	over := h.overflowDir()
	_ = os.MkdirAll(over, 0o700) // mcpshell makes it later; a root must exist
	add(over)
	for f := parentSessionFile(h.cfg.SessionFile); f != ""; f = parentSessionFile(f) {
		add(IsoTmpDir(f))
		add(filepath.Join(filepath.Dir(f), "overflow", strings.TrimSuffix(filepath.Base(f), ".jsonl")))
	}
	return out
}

// parentSessionFile is the session file of a sub-agent's parent
// (childSessionFile reversed), "" for a top-level session.
func parentSessionFile(f string) string {
	m := subSession.FindStringSubmatch(strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	if f == "" || m == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(f), m[1]+".jsonl")
}

// realAbs is an absolute path with its symlinks resolved; a relative one as
// it is.
func realAbs(p string) string {
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		return p
	}
	return resolveExisting(filepath.Clean(p))
}

// tmpReader is the tmp toolset for a read, grep or list call whose path is
// absolute, outside the working tree, and under a session dir; nil otherwise.
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
	real := realAbs(p)
	for _, t := range tmp {
		if _, err := filetools.Under(t.Root(), real); err == nil {
			return t
		}
	}
	return nil
}

// wholeReader answers an unranged read on the toolset it was routed to
// (Harness.readWhole, wholeread.go).
type wholeReader func(ctx context.Context, route *filetools.Toolset, name, args, id string) string

// withFiles routes the file tools to the toolset and reports each call.
// whole, when set, answers a read with no range: all of the file. A path
// outside the workspace that is not open is the person's to open, asked in
// the turn (fileaccess.go).
func withFiles(inner agent.ToolDispatcher, ts *filetools.Toolset, tmp []*filetools.Toolset, h *Harness, whole wholeReader, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if !ts.Handles(tc.Function.Name) {
			return inner(ctx, tc)
		}
		var out, note string
		asked := tc.Function.Arguments // tmp is read by an absolute path, not one fileAccess resolved
		tc.Function.Arguments, out, note = h.fileAccess(ctx, ts, tmp, tc.Function.Name, tc.Function.Arguments)
		sets := h.addedNow() // read per call (and after a grant): see Harness.added
		added := sets.ts
		route := ts
		writes := writesFiles(ts, tc.Function.Name)
		if out == "" {
			if t := tmpReader(ts, tmp, tc.Function.Name, asked); t != nil {
				route = t
			} else if t := addedRoute(ts, added, tc.Function.Arguments); t != nil {
				route = t // a path the person added (/file add, fileadd.go)
			} else if args, t := addedViaLink(ts, added, tc.Function.Arguments); t != nil {
				// The same, named through a symlink: an added path is kept
				// resolved, and the model writes the path the person typed.
				route, tc.Function.Arguments = t, args
			} else if hint := outsideHint(ts, tc.Function.Name, outsidePath(ts, tc.Function.Arguments, "path")); hint != "" {
				out = hint
			}
		}
		// A path open for reading only (/file, mode read): no edit, no write.
		// A file set's mode is the file's: the path, and a move's target.
		if out == "" && writes {
			for _, key := range pathKeys(ts, tc.Function.Name) {
				p := argPath(tc.Function.Arguments, key)
				if p == "" || !sets.readOnlyAt(route, p) {
					continue
				}
				what := route.Root()
				if _, fileSet := sets.writable[route]; fileSet {
					what = filepath.Clean(p)
				}
				out = fmt.Sprintf("ERROR: %s is open to dun for reading only. To change it the user runs /file mode %s write "+
					"(the path, or the directory added). Do not work around this with exec.", what, what)
				break
			}
		}
		// The workspace read only: the tools stay listed and say so, where
		// a list without them sent the model to exec.
		if out == "" && writes && route == ts {
			if write, _ := h.workspaceWrite(); !write {
				out = fmt.Sprintf("ERROR: this workspace (%s) is open to dun for reading only. To change it the user runs "+
					"/file mode . write. Do not work around this with exec.", ts.Root())
			}
		}
		if out == "" && whole != nil && ts.Bare(tc.Function.Name) == filetools.Read && wantsWhole(tc.Function.Arguments) {
			out = whole(ctx, route, tc.Function.Name, tc.Function.Arguments, tc.ID)
		}
		if out == "" {
			out = route.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
		}
		if note != "" {
			out += "\n\n[the user, opening the path: " + note + "]"
		}
		if onCall != nil {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			onCall(tc.Function.Name, args, out)
		}
		return out, nil
	}
}

// systemFiles is the system prompt's line for the file tools. It names no
// paths and no modes (USER 2026-11-07): what is open is the file tools'
// refusal to say and, for exec, the filesystem isolation's to enforce; a
// listed path set is text the model reads every request and can work around.
// It no longer bans cat/sed/grep either: in 72 Terminal-Bench trials the
// model ran cat 179 times, sed 111 and grep 106 beside 190 file_reads. What
// it says is the one reason that holds: sed -i says nothing when its pattern
// misses (exit 0), file_edit refuses (20% vs 10% failed, and sed's count
// misses the silent ones). And the shape of a change (USER 2026-11-18): 18
// exec edits were python or sed making several replacements and running go
// build in the same call; file_edit takes several edits, and the build is a
// call of its own.
func systemFiles(ts *filetools.Toolset) string {
	if ts == nil || !ts.Handles(ts.Name(filetools.Edit)) {
		return ""
	}
	return "\n\nEdit files with " + ts.Name(filetools.Edit) + ": it says when the text did not match; sed -i does not. " +
		"Several changes to one file go in one " + ts.Name(filetools.Edit) + " call (edits: [{old, new}, …]), not a sed or " +
		"python script; run builds and tests in an exec call of their own."
}

// addedRoute is the added path's toolset for a call whose path is absolute
// and under one the person added; nil otherwise.
// addedViaLink is addedRoute for a path that reaches an added one through a
// symlink: /file add keeps the real path (cleanAdded), and a path under the
// link the person typed matched no added root (USER 2026-10-28: "/file add
// DIRECTORY doesn't let the edit tool edit a file of a subdirectory"). It
// returns the arguments with the path resolved, and the toolset.
func addedViaLink(ts *filetools.Toolset, added []*filetools.Toolset, args string) (string, *filetools.Toolset) {
	var a map[string]any
	if json.Unmarshal([]byte(args), &a) != nil {
		return "", nil
	}
	p, _ := a["path"].(string)
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		return "", nil
	}
	real := resolveExisting(filepath.Clean(p))
	if real == filepath.Clean(p) {
		return "", nil
	}
	a["path"] = real
	b, _ := json.Marshal(a)
	if t := addedRoute(ts, added, string(b)); t != nil {
		return string(b), t
	}
	return "", nil
}

// resolveExisting is p with its symlinks resolved: the longest part of it
// that exists, resolved, and the rest (a file about to be written) after it.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

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
