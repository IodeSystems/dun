package ragd

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/raglit"
	"modernc.org/sqlite"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/proto"
)

// docExts are the prose formats the docs index holds; every other file
// goes to code. By extension only, so a file never moves between the two.
// Markdown is cut at its headings (Sections); the rest is read into windows.
var docExts = map[string]bool{
	".md": true, ".markdown": true, ".mdx": true,
	".txt": true, ".rst": true, ".adoc": true, ".pdf": true,
}

// indexFor is "docs" for a prose file, "code" for any other.
func indexFor(path string) string {
	if docExts[strings.ToLower(filepath.Ext(path))] {
		return "docs"
	}
	return "code"
}

// segmenterKey is the index_meta key of the segmenter runtime (codeSeg's
// Runtime) the index's files were last queued under. When it differs from
// the runtime now (the first index with a segmenter, or a new poly-lsp-mcp
// or parser), every file is queued Fresh: raglit skips a file whose bytes
// it already holds, and those fragments were cut by the old segmenter or
// by none.
const segmenterKey = "dund_code_segmenter"

// Index brings q.Project's code and docs indexes in step with files, the
// files chosen under q.Root (dun.IndexFiles: the workspace ingest rules).
// Each file goes to indexFor's index. A file the index does not hold, or
// holds with other bytes (current), is queued as a job for the code+docs
// runner, all of an index's in one transaction (EnqueueMany); a document
// under q.Root that is not in files is removed. An index is created only
// when a file goes to it.
//
// q.Root and files are spelled by fsutil.Canon (canonFiles). When q.Root is
// a linked git worktree (Worktree), each index is its branch over the main
// checkout's (branch.go): the main checkout must be indexed (Indexed), and
// only the branch is written.
func (h *Host) Index(q proto.RagIndex, files []string) (*proto.RagIndexed, error) {
	if q.Project == "" {
		return nil, errors.New("rag: index needs a project")
	}
	if !filepath.IsAbs(q.Root) {
		return nil, fmt.Errorf("rag: %q is not an absolute path", q.Root)
	}
	raw := filepath.Clean(q.Root)
	root := fsutil.Canon(raw)
	files = canonFiles(raw, root, files)
	main, tree := h.Worktree(root)
	if tree && !h.Indexed(q.Project) {
		return nil, fmt.Errorf("rag: %s is a worktree of %s, which %s has not indexed: index it first", root, main, q.Project)
	}
	if !tree {
		main = ""
	}
	split := map[string][]string{}
	for _, f := range files {
		ix := indexFor(f)
		split[ix] = append(split[ix], f)
	}
	out := &proto.RagIndexed{Project: q.Project}
	for _, ix := range []string{"code", "docs"} {
		n, err := h.indexOne(q.Project, ix, root, main, split[ix])
		if err != nil {
			return nil, err
		}
		if ix == "code" {
			out.Code = *n
		} else {
			out.Docs = *n
		}
		out.Queued += n.Queued
		out.Current += n.Current
		out.Removed += n.Removed
		out.Failed += n.Failed
		out.Deferred += n.Deferred
	}
	return out, nil
}

// indexOne brings one index in step with files under root. main is the
// main checkout when root is a worktree ("" when not): the index is then
// the worktree's branch, opened when the parent is on disk.
func (h *Host) indexOne(project, index, root, main string, files []string) (*proto.RagIndexCounts, error) {
	name, err := Name(project, index)
	if err != nil {
		return nil, err
	}
	out := &proto.RagIndexCounts{}
	st, err := h.store(name, len(files) > 0)
	if err != nil || st == nil {
		return out, err
	}
	var parent *raglit.Store
	if main == "" {
		if err := h.dropOutside(name, st, root, out); err != nil {
			return nil, fmt.Errorf("rag: index %s: %w", name, err)
		}
	} else {
		parent = st
		if st, err = h.branch(project, index, root, main, true); err != nil {
			return nil, err
		}
		name = branchName(project, index, root)
	}
	n, err := h.walk(st, parent, root, main, files, out)
	if err != nil {
		if errors.Is(err, raglit.ErrParentOnly) {
			// A branch write raglit refused: dund wrote to a document only the
			// main checkout holds. A bug in this walk, never a skip.
			return nil, fmt.Errorf("rag: index %s: a write to a parent's document: %w", name, err)
		}
		return nil, fmt.Errorf("rag: index %s: %w", name, err)
	}
	return n, nil
}

// outsideKey is the index_meta key dropOutside sets on a main checkout's
// index once it has run: the root it kept.
const outsideKey = "dund_outside_root_dropped"

// dropOutside is a one-time cleanup of st, the code or docs index of the
// main checkout at root. A dund before worktree branches (dda88f1) indexed
// a linked worktree into the main checkout's index and told the two copies
// apart by path; those copies are the documents whose file:// path is not
// under root (both spelled by fsutil.Canon). They are removed, once per
// index (outsideKey). Only when root is a main checkout's top (root/.git is
// a directory): a subdirectory or a directory outside git shares its
// project's index with paths it does not hold. A document that is not a
// file:// path is kept.
func (h *Host) dropOutside(name string, st *raglit.Store, root string, out *proto.RagIndexCounts) error {
	if _, done := st.Meta(outsideKey); done {
		return nil
	}
	if fi, err := os.Stat(filepath.Join(root, ".git")); err != nil || !fi.IsDir() {
		return nil
	}
	docs, err := documents(st)
	if err != nil {
		return err
	}
	n := 0
	for src := range docs {
		p, ok := strings.CutPrefix(src, "file://")
		if !ok {
			continue
		}
		if c := fsutil.Canon(p); c == root || strings.HasPrefix(c, root+"/") {
			continue
		}
		if err := removeBusy(st, src); err != nil {
			return fmt.Errorf("remove %s: %w", src, err)
		}
		n++
	}
	if n > 0 {
		h.logf("dund: rag: %s: removed %d documents outside %s (worktree copies of an older dund)", name, n, root)
	}
	out.Removed += n
	return st.SetMeta(outsideKey, root, time.Now().Unix())
}

// walk is indexOne on st, the index or (parent != nil) the worktree's
// branch over parent.
func (h *Host) walk(st, parent *raglit.Store, root, main string, files []string, out *proto.RagIndexCounts) (*proto.RagIndexCounts, error) {
	docs, err := documents(st) // a branch: its own, then the parent's at worktree paths
	if err != nil {
		return nil, err
	}
	runtime := ""
	if h.code != nil {
		runtime = h.code.Runtime()
	}
	have, _ := st.Meta(segmenterKey)
	fresh := have != runtime && len(docs) > 0
	// A branch re-reads under a new segmenter only the files it holds
	// itself; the parent's are the main checkout's rag_index to re-read.
	var local map[string]bool
	if parent != nil && fresh {
		if local, err = branchOwn(st); err != nil {
			return nil, err
		}
	}
	var failed map[string]int64 // src → when its newest failed job ended
	var waiting map[string]bool // a branch: srcs the parent has a job pending for
	want := make(map[string]bool, len(files))
	var items []raglit.EnqueueItem
	for _, f := range files {
		src := "file://" + f
		want[src] = true
		d, ok := docs[src]
		again := fresh && ok && (parent == nil || local[src])
		if !again && current(d, ok, f) {
			out.Current++
			continue
		}
		if parent != nil {
			if waiting == nil {
				if waiting, err = parentPending(parent, root, main); err != nil {
					return nil, err
				}
			}
			if waiting[src] {
				out.Deferred++
				continue
			}
		}
		if !ok {
			if failed == nil {
				if failed, err = failedJobs(st); err != nil {
					return nil, err
				}
			}
			if at, bad := failed[src]; bad && at >= modTime(f) {
				out.Failed++
				continue
			}
		}
		items = append(items, raglit.EnqueueItem{URL: src, Fresh: again})
	}
	// Removals before the queue, so the runner is not committing this
	// walk's jobs meanwhile (removeBusy).
	under := "file://" + root + "/"
	for src := range docs {
		if !strings.HasPrefix(src, under) || want[src] {
			continue
		}
		// A branch: removing a document only the parent holds tombstones it
		// in the branch; the parent is not touched.
		if err := removeBusy(st, src); err != nil {
			return nil, fmt.Errorf("remove %s: %w", src, err)
		}
		out.Removed++
	}
	if len(items) > 0 {
		res, err := st.EnqueueMany(items)
		if err != nil {
			return nil, fmt.Errorf("queue %d files: %w", len(items), err)
		}
		for _, r := range res {
			if r.Err != nil {
				if errors.Is(r.Err, raglit.ErrIgnored) {
					continue
				}
				return nil, r.Err
			}
			out.Queued++
		}
	}
	if have != runtime {
		if err := st.SetMeta(segmenterKey, runtime, time.Now().Unix()); err != nil {
			return nil, fmt.Errorf("record the segmenter: %w", err)
		}
	}
	return out, nil
}

// removeBusy is removeDoc, again after SQLITE_BUSY (up to 5 tries, 50 ms
// more each time). raglit's RemoveDocument on a branch the runner writes
// returned SQLITE_BUSY at once, not after busy_timeout: 7 of 90 runs of the
// branch tests, 1 of 90 with the removals before the queue (raglit, aw4
// issue). A remove is idempotent, so a retry is safe; the last error is
// returned.
func removeBusy(st *raglit.Store, src string) error {
	var err error
	for i := range 5 {
		if err = removeDoc(st, src); err == nil || !isBusy(err) {
			return err
		}
		time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
	}
	return err
}

// isBusy reports whether err is SQLite's SQLITE_BUSY (any extended code).
func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == 5
}

// branchOwn is the paths of the documents a branch holds itself (not
// tombstones).
func branchOwn(st *raglit.Store) (map[string]bool, error) {
	cs, err := st.BranchDiff()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cs))
	for _, c := range cs {
		if c.Change != "deleted" {
			out[c.Path] = true
		}
	}
	return out, nil
}

// parentPending is the srcs, at worktree paths, that the main checkout's
// index has a pending or running job for.
func parentPending(parent *raglit.Store, root, main string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, state := range []raglit.JobState{raglit.JobPending, raglit.JobRunning} {
		js, err := parent.Jobs(string(state), math.MaxInt32)
		if err != nil {
			return nil, err
		}
		for _, j := range js {
			if rest, ok := strings.CutPrefix(j.URL, "file://"+main+"/"); ok {
				out["file://"+root+"/"+rest] = true
			}
		}
	}
	return out, nil
}

// failedJobs is when the newest failed job of each URL ended (Unix ns).
func failedJobs(st *raglit.Store) (map[string]int64, error) {
	js, err := st.Jobs(string(raglit.JobError), math.MaxInt32)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(js))
	for _, j := range js {
		if _, seen := out[j.URL]; !seen { // newest first
			out[j.URL] = j.FinishedAt
		}
	}
	return out, nil
}

// modTime is path's modified time in Unix ns; 0 when it cannot be read
// (a failure then stands until the file is readable and changes).
func modTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}
