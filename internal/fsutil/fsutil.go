// Package fsutil is what dun needs from the filesystem and the standard library
// does not give it.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic replaces path with data via a temp file in the same
// directory and a rename, so a reader never sees half a file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, perm)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dun: write %s: %w", path, werr)
	}
	return nil
}

// Canon is the one spelling dund stores a root or a path in (docs/rag.md,
// "Path spelling"): absolute p with its symlinks resolved
// (filepath.EvalSymlinks), then Clean. A path that does not exist keeps its
// missing tail on its nearest existing ancestor, resolved. raglit matches
// worktree roots against document paths as strings, so a root and the paths
// under it must be spelled one way; /tmp is a symlink on macOS.
func Canon(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	if dir == p || base == "" {
		return p
	}
	return filepath.Join(Canon(dir), base)
}
