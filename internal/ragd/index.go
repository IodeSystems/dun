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
// runner; a document under q.Root that is not in files is removed. An index
// is created only when a file goes to it.
func (h *Host) Index(q proto.RagIndex, files []string) (*proto.RagIndexed, error) {
	if q.Project == "" {
		return nil, errors.New("rag: index needs a project")
	}
	if !filepath.IsAbs(q.Root) {
		return nil, fmt.Errorf("rag: %q is not an absolute path", q.Root)
	}
	root := filepath.Clean(q.Root)
	split := map[string][]string{}
	for _, f := range files {
		ix := indexFor(f)
		split[ix] = append(split[ix], f)
	}
	out := &proto.RagIndexed{Project: q.Project}
	for _, ix := range []string{"code", "docs"} {
		n, err := h.indexOne(q.Project, ix, root, split[ix])
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
	}
	return out, nil
}

func (h *Host) indexOne(project, index, root string, files []string) (*proto.RagIndexCounts, error) {
	name, err := Name(project, index)
	if err != nil {
		return nil, err
	}
	out := &proto.RagIndexCounts{}
	st, err := h.store(name, len(files) > 0)
	if err != nil || st == nil {
		return out, err
	}
	docs, err := documents(st)
	if err != nil {
		return nil, err
	}
	runtime := ""
	if h.code != nil {
		runtime = h.code.Runtime()
	}
	have, _ := st.Meta(segmenterKey)
	fresh := have != runtime && len(docs) > 0
	var failed map[string]int64 // src → when its newest failed job ended
	want := make(map[string]bool, len(files))
	for _, f := range files {
		src := "file://" + filepath.Clean(f)
		want[src] = true
		d, ok := docs[src]
		if !(fresh && ok) && current(d, ok, f) {
			out.Current++
			continue
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
		if _, err := st.EnqueueFresh(src, "", fresh && ok); err != nil {
			if errors.Is(err, raglit.ErrIgnored) {
				continue
			}
			return nil, err
		}
		out.Queued++
	}
	if have != runtime {
		if err := st.SetMeta(segmenterKey, runtime, time.Now().Unix()); err != nil {
			return nil, err
		}
	}
	under := "file://" + root + "/"
	for src := range docs {
		if !strings.HasPrefix(src, under) || want[src] {
			continue
		}
		if err := removeDoc(st, src); err != nil {
			return nil, err
		}
		out.Removed++
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
