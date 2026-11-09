package ragd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/proto"
)

// Worktree branches (docs/rag.md, "Worktrees"; USER 2026-11-09: worktrees
// are raglit branches).
//
// The code and docs indexes of a project hold its main checkout. A linked
// git worktree of it (raglit.WorktreeOf) gets a branch of each, opened with
// raglit.OpenBranch over the main checkout's index and named after the
// worktree:
//
//	<project>/branches/<worktree>/{code,docs}/
//
// The branch stores only the files that differ from the main checkout's
// (and tombstones for the ones the worktree deleted); every other file
// shows through from the parent, at its worktree path. rag_index on a
// worktree writes to the branch only, and a search with a worktree root
// reads the branch.
//
// Every root and path dund gives raglit is spelled by fsutil.Canon: raglit
// translates paths by string prefix, so one spelling is the contract.

// branchesDir is the directory of a project's branches.
const branchesDir = "branches"

// branchName is the index key (and directory under the rag root) of
// project's index branch for the worktree at root: the worktree's base name
// as a slug and a short hash of root, so two worktrees with one base name
// do not share a branch.
func branchName(project, index, root string) string {
	sum := sha256.Sum256([]byte(root))
	return project + "/" + branchesDir + "/" + slugOf(filepath.Base(root)) + "-" + hex.EncodeToString(sum[:4]) + "/" + index
}

// slugOf is s in the characters checkSlug allows; "wt" when none are left.
func slugOf(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if out == "" {
		return "wt"
	}
	return out
}

// indexOf is the index kind of an index key: its last element ("code" of
// "dun/code" and of "dun/branches/x-1a2b3c4d/code").
func indexOf(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// parentOf is the parent index key of a branch key: "dun/code" of
// "dun/branches/x-1a2b3c4d/code".
func parentOf(name string) string {
	project, _, _ := strings.Cut(name, "/")
	return project + "/" + indexOf(name)
}

// Worktree is the main checkout of the linked git worktree at root, both
// spelled by fsutil.Canon; ok is false when root is not a linked worktree.
// The answer is kept per root: a directory does not change between a main
// checkout and a worktree while it exists.
func (h *Host) Worktree(root string) (main string, ok bool) {
	root = fsutil.Canon(root)
	h.mu.Lock()
	m, seen := h.trees[root]
	h.mu.Unlock()
	if seen {
		return m, m != ""
	}
	if m, ok = raglit.WorktreeOf(root); ok {
		m = fsutil.Canon(m)
	}
	h.mu.Lock()
	h.trees[root] = m
	h.mu.Unlock()
	return m, ok
}

// Indexed reports whether project has a code or a docs index on disk: its
// main checkout was indexed.
func (h *Host) Indexed(project string) bool {
	for _, ix := range []string{"code", "docs"} {
		if name, err := Name(project, ix); err == nil && h.onDisk(name) {
			return true
		}
	}
	return false
}

func (h *Host) onDisk(name string) bool {
	_, err := os.Stat(raglit.Home(filepath.Join(h.root, name)).IndexPath())
	return err == nil
}

// branch is project's index branch for the worktree root of main, both
// canonical. create opens (and makes) it; without create, a branch not on
// disk is (nil, nil). The parent index must be on disk.
func (h *Host) branch(project, index, root, main string, create bool) (*raglit.Store, error) {
	name := branchName(project, index, root)
	if st, err := h.Existing(name); err == nil {
		return st, nil
	}
	if !create && !h.onDisk(name) {
		return nil, nil
	}
	return h.openBranch(name, root, main)
}

// openBranch opens the branch name over its parent with raglit.OpenBranch
// and gives it what open gives an index.
func (h *Host) openBranch(name, root, main string) (*raglit.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("rag: dund is stopping")
	}
	if st, ok := h.stores[name]; ok {
		return st, nil
	}
	parent := raglit.Home(filepath.Join(h.root, parentOf(name)))
	st, err := raglit.OpenBranch(parent, raglit.Home(filepath.Join(h.root, name)), root, main)
	if err != nil {
		return nil, fmt.Errorf("rag: open branch %s: %w", name, err)
	}
	h.attachEmbed(name, st)
	st.SetIdentifier(h.ident)
	h.stores[name] = st
	return st, nil
}

// branchHomes is every branch home under the rag root:
// <root>/<project>/branches/<worktree>/{code,docs}, the ones that exist.
func branchHomes(root string) []string {
	var out []string
	for _, ix := range []string{"code", "docs"} {
		m, _ := filepath.Glob(filepath.Join(root, "*", branchesDir, "*", ix))
		out = append(out, m...)
	}
	return out
}

// branchMeta reads a branch home's branch.json (raglit.BranchMeta, which
// raglit writes in the branch's home); ok is false when there is none.
func branchMeta(home string) (raglit.BranchMeta, bool) {
	b, err := os.ReadFile(filepath.Join(home, "branch.json"))
	if err != nil {
		return raglit.BranchMeta{}, false
	}
	var m raglit.BranchMeta
	if json.Unmarshal(b, &m) != nil {
		return raglit.BranchMeta{}, false
	}
	return m, true
}

// rootAlive is GCBranches's alive: the worktree root is still a directory.
func rootAlive(root string) bool {
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}

// openBranches opens every branch on disk whose parent index is, at start.
// A branch that cannot be opened is logged and left closed: its worktree's
// next rag_index or search says why.
func (h *Host) openBranches() {
	for _, home := range branchHomes(h.root) {
		m, ok := branchMeta(home)
		if !ok || m.Root == "" {
			continue
		}
		name, err := filepath.Rel(h.root, home)
		if err != nil {
			continue
		}
		name = filepath.ToSlash(name)
		if !h.onDisk(parentOf(name)) {
			h.logf("dund: rag: branch %s: its parent %s is not on disk; left closed", name, parentOf(name))
			continue
		}
		if _, err := h.openBranch(name, m.Root, m.ParentRoot); err != nil {
			h.logf("dund: rag: %v; left closed", err)
		}
	}
}

// GC removes every index branch whose worktree root no longer exists
// (raglit.GCBranches over every branch home, alive: the root is a
// directory). A dead branch that is open is closed first; a job running on
// it fails, as it would on its deleted files.
func (h *Host) GC() (*proto.RagGCed, error) {
	homes := branchHomes(h.root)
	out := &proto.RagGCed{Branches: len(homes)}
	h.mu.Lock()
	for _, home := range homes {
		m, ok := branchMeta(home)
		if !ok || m.Root == "" || rootAlive(m.Root) {
			continue
		}
		name, err := filepath.Rel(h.root, home)
		if err != nil {
			continue
		}
		name = filepath.ToSlash(name)
		if st, ok := h.stores[name]; ok {
			_ = st.Close()
			delete(h.stores, name)
			delete(h.stale, name)
		}
		delete(h.trees, m.Root)
	}
	h.mu.Unlock()
	hs := make([]raglit.Home, len(homes))
	for i, home := range homes {
		hs[i] = raglit.Home(home)
	}
	n, err := raglit.GCBranches(hs, rootAlive)
	out.Removed = n
	for _, home := range homes {
		_ = os.Remove(filepath.Dir(home)) // the worktree's directory, once empty
	}
	if n > 0 {
		h.logf("dund: rag: removed %d index branches whose worktree is gone", n)
	}
	if err != nil {
		return out, fmt.Errorf("rag: gc: %w", err)
	}
	return out, nil
}

// canonPrefix is a search path prefix spelled by fsutil.Canon: a file://
// URL or an absolute path, its trailing "/" kept. Anything else as given.
func canonPrefix(p string) string {
	scheme, rest := "", p
	if r, ok := strings.CutPrefix(p, "file://"); ok {
		scheme, rest = "file://", r
	}
	if !filepath.IsAbs(rest) {
		return p
	}
	c := fsutil.Canon(rest)
	if strings.HasSuffix(rest, "/") && c != "/" {
		c += "/"
	}
	return scheme + c
}

// canonFiles spells files by root's canonical form: a file under raw (the
// root as given) is moved under root (fsutil.Canon of raw), keeping its
// path below the root as walked; any other file is fsutil.Canon of itself.
func canonFiles(raw, root string, files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		f = filepath.Clean(f)
		switch {
		case raw == root:
			out[i] = f
		case f == raw:
			out[i] = root
		case strings.HasPrefix(f, raw+"/"):
			out[i] = root + f[len(raw):]
		default:
			out[i] = fsutil.Canon(f)
		}
	}
	return out
}
