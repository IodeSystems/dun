package dun

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The root guard (2026-11-09): a backstop, in dun, before poly-lsp-mcp or
// dund's code index takes a whole root.
//
// On a Mac, dun ran in a directory of cloned repositories; the model's
// node_* calls had poly-lsp index all of it, and the machine went down
// twice. poly-lsp v0.5.1 brings its own caps; this guard stays correct
// with them or without them, and costs one bounded count per root.
//
// ScanRoot counts, quickly and to a cap:
//
//   - in a git work tree: the files git lists (tracked, and untracked that
//     are not ignored), counting stopped at guardMaxFiles+1. The root is one
//     repository: git decides the file set, and a nested clone is one entry.
//   - outside one: a walk that counts files to the same cap, skipping what
//     poly-lsp's own walk skips (guardSkipDirs), and names the git
//     repositories to depth guardRepoDepth.
//
// Either stops at guardScanTimeout; a scan that ran out of time is over.
//
// A root with guardMaxRepos repositories or more, or over guardMaxFiles
// files, is too big. Then:
//
//   - poly-lsp (guardCodeRoot): it starts on an empty placeholder root, so
//     its tools are listed and nothing is indexed. A node_* call that names
//     a path inside one repository restarts it on that repository
//     (withCodeGuard) and is passed on with the paths made relative to it.
//     A call that names none, or more than one, is refused with the list of
//     repositories. A repository that is itself too big is refused.
//   - dund's code index (codeIndexRoot): the root is not indexed.
//
// The person's /lsp index-anyway on (ServerSpec.IndexAnyway) lifts the
// poly-lsp guard: it is their explicit choice to index the root whole.

// The guard's limits.
const (
	guardMaxRepos    = 3
	guardRepoDepth   = 3
	guardScanTimeout = 3 * time.Second
)

// guardMaxFiles is the file cap; a var so a test can lower it.
var guardMaxFiles = 50000

// guardSkipDirs are the directories the walk does not count, as
// poly-lsp-mcp's index does not descend into them (symbols.skipDirs).
var guardSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "__pycache__": true, "dist": true, "build": true,
	".idea": true, ".vscode": true, ".gradle": true, "Pods": true, "Carthage": true, ".build": true,
	".swiftpm": true, "DerivedData": true, "xcuserdata": true,
}

// RootScan is what ScanRoot found under a root.
type RootScan struct {
	Root string
	// InRepo: the root is in a git work tree; git listed its files.
	InRepo bool
	// Repos are the git repositories under a root outside one, to depth
	// guardRepoDepth, relative and slash-separated, sorted.
	Repos []string
	// Files is the files counted, at most guardMaxFiles+1.
	Files int
	// Over: counting stopped at the cap, or the scan ran out of time
	// (TimedOut).
	Over     bool
	TimedOut bool
	Took     time.Duration
}

// TooBig reports whether the root is not indexed whole.
func (r RootScan) TooBig() bool { return len(r.Repos) >= guardMaxRepos || r.Over }

// rootScans caches ScanRoot by root for rootScanTTL: a session start asks
// for poly-lsp and the code index at once, and a restart asks again.
var rootScans sync.Map // root → rootScanEntry

type rootScanEntry struct {
	at   time.Time
	scan RootScan
}

const rootScanTTL = time.Minute

// scanRootCached is ScanRoot, from the cache when it is fresh.
func scanRootCached(root string) RootScan {
	root = filepath.Clean(root)
	if v, ok := rootScans.Load(root); ok {
		if e := v.(rootScanEntry); time.Since(e.at) < rootScanTTL {
			return e.scan
		}
	}
	s := ScanRoot(root)
	rootScans.Store(root, rootScanEntry{at: time.Now(), scan: s})
	return s
}

// ScanRoot counts root's repositories and files, to the guard's caps.
func ScanRoot(root string) RootScan {
	t0 := time.Now()
	root = filepath.Clean(root)
	out := RootScan{Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), guardScanTimeout)
	defer cancel()
	if inGitTree(root) {
		if n, over, err := gitFileCount(ctx, root); err == nil {
			out.InRepo, out.Files, out.Over = true, n, over
			out.TimedOut = ctx.Err() != nil
			out.Over = out.Over || out.TimedOut
			out.Took = time.Since(t0)
			return out
		}
		// No git, or it failed: count by walking.
	}
	walkRoot(ctx, root, &out)
	out.Took = time.Since(t0)
	return out
}

// inGitTree reports whether dir or a parent holds .git.
func inGitTree(dir string) bool {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		p := filepath.Dir(d)
		if p == d {
			return false
		}
		d = p
	}
}

// gitFileCount counts the files git lists under root (tracked, and
// untracked not ignored), stopping at guardMaxFiles+1.
func gitFileCount(ctx context.Context, root string) (n int, over bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, false, err
	}
	if err := cmd.Start(); err != nil {
		return 0, false, err
	}
	buf := make([]byte, 64<<10)
	for {
		k, rerr := out.Read(buf)
		n += bytes.Count(buf[:k], []byte{0})
		if n > guardMaxFiles {
			over = true
			break
		}
		if rerr != nil {
			break
		}
	}
	if over || ctx.Err() != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return min(n, guardMaxFiles+1), true, nil
	}
	if err := cmd.Wait(); err != nil {
		return 0, false, err
	}
	return n, false, nil
}

// walkRoot counts files under root to the cap and names the repositories
// to guardRepoDepth. Past the cap it goes on only to find repositories,
// no deeper than guardRepoDepth.
func walkRoot(ctx context.Context, root string, out *RootScan) {
	seen := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if seen++; seen%512 == 0 && ctx.Err() != nil {
			out.Over, out.TimedOut = true, true
			return filepath.SkipAll
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		name := d.Name()
		if name == ".git" {
			if parent := filepath.Dir(rel); parent != "." && depth-1 <= guardRepoDepth {
				out.Repos = append(out.Repos, filepath.ToSlash(parent))
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if guardSkipDirs[name] || (out.Over && depth > guardRepoDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if out.Over {
			return nil
		}
		if d.Type().IsRegular() {
			out.Files++
			if out.Files > guardMaxFiles {
				out.Over = true
			}
		}
		return nil
	})
	sort.Strings(out.Repos)
}
