package dund

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// ragIndex walks q.Root with the workspace ingest rules (dun.IndexFiles,
// which ragd cannot import) and hands the files to the code and docs
// indexes. An empty project is the root's slug.
func ragIndex(h *ragd.Host, q proto.RagIndex) (*proto.RagIndexed, error) {
	if !filepath.IsAbs(q.Root) {
		return nil, fmt.Errorf("rag: %q is not an absolute path", q.Root)
	}
	if fi, err := os.Stat(q.Root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("rag: %s is not a directory", q.Root)
	}
	if q.Project == "" {
		q.Project = dun.ProjectSlug(q.Root)
	}
	files, summary := dun.IndexFiles(q.Root)
	out, err := h.Index(q, files)
	if err != nil {
		return nil, err
	}
	out.Summary = summary
	return out, nil
}
