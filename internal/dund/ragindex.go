package dund

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// ragIndex walks q.Root with the workspace ingest rules (dun.IndexFiles,
// which ragd cannot import) and hands the files to the code and docs
// indexes. An empty project is the root's slug. The root is walked as
// fsutil.Canon spells it, so every path under it is spelled one way. A
// linked worktree whose main checkout the project has not indexed gets the
// main checkout indexed first: the worktree's branch is over that index.
func ragIndex(h *ragd.Host, q proto.RagIndex) (*proto.RagIndexed, error) {
	if !filepath.IsAbs(q.Root) {
		return nil, fmt.Errorf("rag: %q is not an absolute path", q.Root)
	}
	if fi, err := os.Stat(q.Root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("rag: %s is not a directory", q.Root)
	}
	q.Root = fsutil.Canon(q.Root)
	if q.Project == "" {
		q.Project = dun.ProjectSlug(q.Root)
	}
	if main, ok := h.Worktree(q.Root); ok && !h.Indexed(q.Project) {
		files, _ := dun.IndexFiles(main)
		if _, err := h.Index(proto.RagIndex{Project: q.Project, Root: main}, files); err != nil {
			return nil, fmt.Errorf("rag: index the main checkout %s first: %w", main, err)
		}
	}
	files, summary := dun.IndexFiles(q.Root)
	out, err := h.Index(q, files)
	if err != nil {
		return nil, err
	}
	out.Summary = summary
	return out, nil
}
