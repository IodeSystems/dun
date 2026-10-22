package dun

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// What the docs index is given (USER 2026-10-22: "a problem with extremely
// large files and rag indexing"). raglit extracts, segments and indexes
// whatever it is handed and has no size defence of its own, so a workspace
// with a node_modules, a data dump or a 300 MB JSON file costs minutes of
// ingest and an index full of noise.
//
// The rules, applied to one file list (git's, or a walk outside git):
//
//   - dependency, build and cache directories by NAME, anywhere (junkDirs);
//   - a directory whose files would add more than max_dir_files or
//     max_dir_mb, skipped whole at the HIGHEST such directory — never the
//     workspace root itself;
//   - files over max_file_kb, and binary files (a known extension, or a NUL
//     byte in the first 8 KB);
//   - outside git, .gitignore files are honored as git would.
//
// Every skip is reported in one summary line with the include that undoes it.
//
// Config, in any dun.json layer:
//
//	{"rag": {"include": ["data/"], "exclude": ["fixtures/"],
//	         "max_file_kb": 1024, "max_dir_files": 5000, "max_dir_mb": 200}}
//
// include and exclude are gitignore-syntax patterns, accumulated across
// layers. include overrides every automatic skip (not .gitignore); exclude
// wins over include. Inside a dependency or build directory only an include
// naming that directory or a path in it ("node_modules/foo/") reaches files —
// a bare glob like "*.json" does not reopen every node_modules.

// RagConfig is the "rag" section of dun.json.
type RagConfig struct {
	Include     []string `json:"include,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
	MaxFileKB   int      `json:"max_file_kb,omitempty"`
	MaxDirFiles int      `json:"max_dir_files,omitempty"`
	MaxDirMB    int      `json:"max_dir_mb,omitempty"`
}

const (
	defaultRagMaxFileKB   = 1024
	defaultRagMaxDirFiles = 5000
	defaultRagMaxDirMB    = 200
)

// LoadRag resolves the effective rag config across the same layers as
// LoadFiles. nil means no section anywhere: the defaults.
func LoadRag(dir string) *RagConfig {
	var out *RagConfig
	for _, p := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(p)
		if err != nil || f == nil || f.Rag == nil {
			continue
		}
		if out == nil {
			cp := *f.Rag
			out = &cp
			continue
		}
		merged := mergeRag(*out, *f.Rag)
		out = &merged
	}
	return out
}

// mergeRag layers next over prev: limits next states win, patterns add up.
func mergeRag(prev, next RagConfig) RagConfig {
	prev.Include = append(append([]string(nil), prev.Include...), next.Include...)
	prev.Exclude = append(append([]string(nil), prev.Exclude...), next.Exclude...)
	if next.MaxFileKB != 0 {
		prev.MaxFileKB = next.MaxFileKB
	}
	if next.MaxDirFiles != 0 {
		prev.MaxDirFiles = next.MaxDirFiles
	}
	if next.MaxDirMB != 0 {
		prev.MaxDirMB = next.MaxDirMB
	}
	return prev
}

func (c *RagConfig) limits() (maxFile int64, maxDirFiles int, maxDirBytes int64) {
	kb, files, mb := defaultRagMaxFileKB, defaultRagMaxDirFiles, defaultRagMaxDirMB
	if c != nil {
		if c.MaxFileKB > 0 {
			kb = c.MaxFileKB
		}
		if c.MaxDirFiles > 0 {
			files = c.MaxDirFiles
		}
		if c.MaxDirMB > 0 {
			mb = c.MaxDirMB
		}
	}
	return int64(kb) << 10, files, int64(mb) << 20
}

// junkDirs are skipped by name anywhere in the tree; the value is the reason
// the summary gives. env and target are conditional (see junkReason).
var junkDirs = map[string]string{
	"node_modules": "dependencies", "bower_components": "dependencies", "Pods": "dependencies",
	".venv": "virtualenv", "venv": "virtualenv",
	"__pycache__": "cache", ".tox": "cache", ".mypy_cache": "cache", ".pytest_cache": "cache",
	".cache": "cache", ".gradle": "cache", ".terraform": "cache",
	"dist": "build output", "build": "build output", "out": "build output",
	".next": "build output", ".nuxt": "build output", ".svelte-kit": "build output",
	"DerivedData": "build output", "coverage": "coverage", ".idea": "editor state",
	".git": "git",
}

// workspaceCopies are dun's (and Claude Code's) own worktrees: copies of the
// workspace, which would fill every search with stale duplicates.
var workspaceCopies = map[string]bool{".dun/worktrees": true, ".claude/worktrees": true}

// SkippedDir is a directory left out of the index. Reason is a junk class
// ("dependencies", ...) or "" for the size rule; Files/Bytes are what it held
// (unknown, zero, for a junk directory the walk did not enter).
type SkippedDir struct {
	Rel    string
	Reason string
	Files  int
	Bytes  int64
}

// SkippedFile is one file over the size limit.
type SkippedFile struct {
	Rel   string
	Bytes int64
}

// IngestPlan is what the docs index is given, and what it is not.
type IngestPlan struct {
	Files    []string // absolute paths
	Dirs     []SkippedDir
	BigFiles []SkippedFile // largest first
	Binary   int
	// Huge means what is indexed is itself over the directory limits: the
	// root is never skipped, so a big repo is said to be one rather than
	// silently ingested. KeptBytes is the indexed total.
	KeptBytes int64
	Huge      bool
	MaxFile   int64
}

type ingestEntry struct {
	rel  string // slash-separated, relative to the workspace
	size int64
}

// planIngest decides what raglit indexes in workspace.
func planIngest(workspace string, cfg *RagConfig) IngestPlan {
	maxFile, maxDirFiles, maxDirBytes := cfg.limits()
	plan := IngestPlan{MaxFile: maxFile}
	var inc, exc *globRules
	if cfg != nil {
		inc, exc = newGlobRules("", cfg.Include), newGlobRules("", cfg.Exclude)
	}
	incPaths := inc.anchored() // what may reach inside a junk directory
	jr := &junkJudge{ws: workspace, memo: map[string]string{}}

	entries, pruned, ok := gitEntries(workspace)
	if !ok {
		var err error
		entries, pruned, err = walkEntries(workspace, inc, exc, jr)
		if err != nil {
			// Unreadable: hand over the root and let raglit say why, rather
			// than silently indexing nothing.
			plan.Files = []string{workspace}
			return plan
		}
	}

	junk := map[string]*SkippedDir{}
	for rel, why := range pruned {
		junk[rel] = &SkippedDir{Rel: rel, Reason: why}
	}
	var kept, cands []ingestEntry
	for _, e := range entries {
		if exc.matches(e.rel, false) {
			continue
		}
		// include overrides every automatic skip; inside a junk directory,
		// only an include naming it or a path in it does.
		j, why := jr.topJunk(e.rel)
		if j == "" && inc.matches(e.rel, false) ||
			j != "" && (inc.matches(j, true) || incPaths.matches(e.rel, false)) {
			kept = append(kept, e)
			continue
		}
		if j != "" {
			d := junk[j]
			if d == nil {
				d = &SkippedDir{Rel: j, Reason: why}
				junk[j] = d
			}
			d.Files++
			d.Bytes += e.size
			continue
		}
		if e.size > maxFile {
			plan.BigFiles = append(plan.BigFiles, SkippedFile{e.rel, e.size})
			continue
		}
		if binaryExt(e.rel) {
			plan.Binary++
			continue
		}
		cands = append(cands, e)
	}

	// The directory rule, over what would otherwise be indexed: aggregate
	// every ancestor in one pass, then skip top-down at the first directory
	// over a limit, so a skipped data/ is reported once, not with every
	// subdirectory of it.
	type agg struct {
		files int
		bytes int64
	}
	dirs := map[string]*agg{}
	for _, e := range cands {
		for i := 0; i < len(e.rel); i++ {
			if e.rel[i] == '/' {
				a := dirs[e.rel[:i]]
				if a == nil {
					a = &agg{}
					dirs[e.rel[:i]] = a
				}
				a.files++
				a.bytes += e.size
			}
		}
	}
	order := make([]string, 0, len(dirs))
	for d := range dirs {
		order = append(order, d)
	}
	sort.Slice(order, func(i, j int) bool {
		di, dj := strings.Count(order[i], "/"), strings.Count(order[j], "/")
		if di != dj {
			return di < dj
		}
		return order[i] < order[j]
	})
	skipped := map[string]bool{}
	for _, d := range order {
		if inSkipped(d, skipped) {
			continue
		}
		if a := dirs[d]; a.files > maxDirFiles || a.bytes > maxDirBytes {
			skipped[d] = true
			plan.Dirs = append(plan.Dirs, SkippedDir{Rel: d, Files: a.files, Bytes: a.bytes})
		}
	}
	for _, e := range cands {
		if len(skipped) > 0 && inSkipped(e.rel, skipped) {
			continue
		}
		if sniffBinary(filepath.Join(workspace, filepath.FromSlash(e.rel))) {
			plan.Binary++
			continue
		}
		kept = append(kept, e)
	}

	// Junk first (named), then size-skipped directories largest first.
	var junkList []SkippedDir
	for _, d := range junk {
		junkList = append(junkList, *d)
	}
	sort.Slice(junkList, func(i, j int) bool { return junkList[i].Rel < junkList[j].Rel })
	sort.SliceStable(plan.Dirs, func(i, j int) bool { return plan.Dirs[i].Bytes > plan.Dirs[j].Bytes })
	plan.Dirs = append(junkList, plan.Dirs...)
	sort.Slice(plan.BigFiles, func(i, j int) bool { return plan.BigFiles[i].Bytes > plan.BigFiles[j].Bytes })

	sort.Slice(kept, func(i, j int) bool { return kept[i].rel < kept[j].rel })
	for _, e := range kept {
		plan.KeptBytes += e.size
		plan.Files = append(plan.Files, filepath.Join(workspace, filepath.FromSlash(e.rel)))
	}
	plan.Huge = len(kept) > maxDirFiles || plan.KeptBytes > maxDirBytes
	return plan
}

// inSkipped reports whether rel is inside (or is) one of dirs.
func inSkipped(rel string, dirs map[string]bool) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && dirs[rel[:i]] {
			return true
		}
	}
	return dirs[rel]
}

// junkJudge classifies directories, with the filesystem checks the
// conditional names need, memoized.
type junkJudge struct {
	ws   string
	memo map[string]string
}

// reason is why dir (slash, relative) is junk, "" if it is not.
func (j *junkJudge) reason(dir string) string {
	if why, ok := j.memo[dir]; ok {
		return why
	}
	why := ""
	name := path.Base(dir)
	switch {
	case workspaceCopies[dir]:
		why = "worktrees"
	case junkDirs[name] != "":
		why = junkDirs[name]
	case name == "env":
		// A Python virtualenv; an env/ of config files is real content.
		if exists(filepath.Join(j.ws, filepath.FromSlash(dir), "pyvenv.cfg")) {
			why = "virtualenv"
		}
	case name == "target":
		// Cargo and Maven build output; a target/ elsewhere is just a name.
		parent := filepath.Join(j.ws, filepath.FromSlash(path.Dir(dir)))
		if exists(filepath.Join(parent, "Cargo.toml")) || exists(filepath.Join(parent, "pom.xml")) {
			why = "build output"
		}
	}
	j.memo[dir] = why
	return why
}

// topJunk is the outermost junk directory rel lies in.
func (j *junkJudge) topJunk(rel string) (string, string) {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			if why := j.reason(rel[:i]); why != "" {
				return rel[:i], why
			}
		}
	}
	return "", ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gitEntries lists what git would show — tracked files plus untracked ones
// .gitignore does not exclude. false when the workspace is not a git repo.
func gitEntries(workspace string) ([]ingestEntry, map[string]string, bool) {
	out, err := exec.Command("git", "-C", workspace, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, nil, false
	}
	var entries []ingestEntry
	seen := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		fi, err := os.Lstat(filepath.Join(workspace, filepath.FromSlash(rel)))
		if err != nil || !fi.Mode().IsRegular() {
			continue // deleted, a symlink, a submodule
		}
		entries = append(entries, ingestEntry{rel, fi.Size()})
	}
	if len(entries) == 0 {
		return nil, nil, false
	}
	return entries, nil, true
}

// walkEntries lists a workspace that is not a git repo, honoring .gitignore
// files as git would and not descending into junk or excluded directories at
// all — the walk is where a node_modules costs its time. It returns the junk
// directories it did not enter.
func walkEntries(workspace string, inc, exc *globRules, jr *junkJudge) ([]ingestEntry, map[string]string, error) {
	ign := &globRules{}
	pruned := map[string]string{}
	var entries []ingestEntry
	err := filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == workspace {
				return err
			}
			return nil // an unreadable corner costs that corner
		}
		rel, _ := filepath.Rel(workspace, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				ign.addFile("", filepath.Join(p, ".gitignore"))
				return nil
			}
			if d.Name() == ".git" || ign.matches(rel, true) || exc.matches(rel, true) {
				return filepath.SkipDir
			}
			if why := jr.reason(rel); why != "" && !inc.reaches(rel) {
				if _, inJunk := jr.topJunk(rel); inJunk == "" {
					pruned[rel] = why
				}
				return filepath.SkipDir
			}
			ign.addFile(rel, filepath.Join(p, ".gitignore"))
			return nil
		}
		if !d.Type().IsRegular() || ign.matches(rel, false) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, ingestEntry{rel, fi.Size()})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return entries, pruned, nil
}

// binaryExts are formats that are binary by construction. PDF is not one:
// raglit reads PDFs (OCR).
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".ico": true,
	".webp": true, ".tif": true, ".tiff": true, ".psd": true, ".heic": true,
	".mp3": true, ".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".wav": true,
	".flac": true, ".ogg": true, ".webm": true, ".m4a": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true,
	".7z": true, ".rar": true, ".tar": true, ".jar": true, ".war": true, ".whl": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true, ".o": true,
	".obj": true, ".class": true, ".pyc": true, ".pyo": true, ".wasm": true, ".bin": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".sqlite": true, ".sqlite3": true, ".db": true, ".parquet": true, ".npy": true,
	".npz": true, ".pkl": true, ".pt": true, ".onnx": true, ".safetensors": true,
	".iso": true, ".dmg": true,
}

func binaryExt(rel string) bool {
	return binaryExts[strings.ToLower(path.Ext(rel))]
}

// sniffBinary reports a NUL byte in the first 8 KB — git's own test.
func sniffBinary(p string) bool {
	if strings.EqualFold(filepath.Ext(p), ".pdf") {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 8<<10)
	n, _ := io.ReadFull(f, buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// summaryDirs is how many skipped directories the summary names.
const summaryDirs = 5

// Summary is the line the user sees: what is indexed, what was skipped and
// why, and the include that brings a skipped path back.
func (p IngestPlan) Summary() string {
	var b strings.Builder
	b.WriteString("docs: indexing " + Grouped(len(p.Files)) + " file" + plural(len(p.Files)))
	if p.Huge {
		fmt.Fprintf(&b, " (%s, over the directory limit — the workspace itself is large)", HumanBytes(p.KeptBytes))
	}
	var skips []string
	var shown []SkippedDir
	for _, d := range p.Dirs {
		if d.Reason != "git" {
			shown = append(shown, d)
		}
	}
	var names []string
	for i, d := range shown {
		if i == summaryDirs {
			names = append(names, fmt.Sprintf("+%d more", len(shown)-summaryDirs))
			break
		}
		if d.Reason != "" {
			names = append(names, d.Rel+"/ ("+d.Reason+")")
		} else {
			names = append(names, fmt.Sprintf("%s/ (%s files, %s)", d.Rel, Grouped(d.Files), HumanBytes(d.Bytes)))
		}
	}
	if len(names) > 0 {
		skips = append(skips, strings.Join(names, ", "))
	}
	if n := len(p.BigFiles); n > 0 {
		var big []string
		for i, f := range p.BigFiles {
			if i == 3 {
				break
			}
			big = append(big, path.Base(f.Rel)+" "+HumanBytes(f.Bytes))
		}
		skips = append(skips, fmt.Sprintf("%s %s over %s (largest: %s)",
			Grouped(n), "file"+plural(n), HumanBytes(p.MaxFile), strings.Join(big, ", ")))
	}
	if p.Binary > 0 {
		skips = append(skips, Grouped(p.Binary)+" binary file"+plural(p.Binary))
	}
	if len(skips) == 0 {
		return b.String()
	}
	b.WriteString(" · skipped " + strings.Join(skips, ", "))
	// The example is a size-skipped directory when there is one: that is
	// more often content than a node_modules is.
	example := ""
	for _, d := range shown {
		if d.Reason == "" {
			example = d.Rel + "/"
			break
		}
	}
	switch {
	case example != "":
	case len(shown) > 0:
		example = shown[0].Rel + "/"
	case len(p.BigFiles) > 0:
		example = p.BigFiles[0].Rel
	}
	if example != "" {
		fmt.Fprintf(&b, ` — to include one: "rag": {"include": [%q]} in dun.json`, example)
	}
	return b.String()
}
