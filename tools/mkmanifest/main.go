// mkmanifest writes a release's dun-manifest.json (make release): dun's
// entries from the archives in -dist (dun_<os>_<arch>.tar.gz, sha256 and
// size computed here, URL -url/<archive>), the tools' entries copied from
// -deps (release/deps.json, the same file the release builds embed), and
// key_next from -key-next when that file exists. It signs nothing: make
// release runs the minisign CLI on its output.
//
// The URL comes from the Makefile, not from here: no Go source outside a
// release-tagged file names a download location (owner, item 47).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/update"
)

func main() {
	version := flag.String("version", "", "the release version, vX.Y.Z")
	dist := flag.String("dist", "", "the directory holding dun_<os>_<arch>.tar.gz")
	deps := flag.String("deps", "release/deps.json", "the tools manifest (mcpshell, poly-lsp-mcp)")
	base := flag.String("url", "", "the release's download URL; each archive is <url>/<archive>")
	keyNext := flag.String("key-next", "release/key_next.json", "a next-key endorsement {key, signature}, when the file exists")
	summary := flag.String("summary", "", "one line on what changed")
	out := flag.String("o", "", "where to write the manifest")
	flag.Parse()
	if err := run(*version, *dist, *deps, *base, *keyNext, *summary, *out); err != nil {
		fmt.Fprintln(os.Stderr, "mkmanifest:", err)
		os.Exit(1)
	}
}

func run(version, dist, deps, base, keyNext, summary, out string) error {
	if _, err := update.ParseVersion(version); err != nil {
		return err
	}
	if dist == "" || base == "" || out == "" {
		return errors.New("-dist, -url and -o are required")
	}
	b, err := os.ReadFile(deps)
	if err != nil {
		return err
	}
	m, err := update.ParseManifest(b)
	if err != nil {
		return fmt.Errorf("%s: %w", deps, err)
	}
	m.Version, m.Summary = version, summary
	m.Published = time.Now().UTC().Format(time.RFC3339)
	if m.Artifacts == nil {
		m.Artifacts = map[string]update.Artifact{}
	}
	dunArt := update.Artifact{Version: version, Files: map[string]update.File{}}
	archives, _ := filepath.Glob(filepath.Join(dist, "dun_*_*.tar.gz"))
	if len(archives) == 0 {
		return fmt.Errorf("no dun_<os>_<arch>.tar.gz in %s", dist)
	}
	for _, a := range archives {
		name := filepath.Base(a)
		plat := strings.TrimSuffix(strings.TrimPrefix(name, "dun_"), ".tar.gz")
		sum, size, err := hashFile(a)
		if err != nil {
			return err
		}
		dunArt.Files[plat] = update.File{URL: strings.TrimSuffix(base, "/") + "/" + name, SHA256: sum, Size: size}
	}
	m.Artifacts["dun"] = dunArt
	if kb, err := os.ReadFile(keyNext); err == nil {
		var e update.Endorsement
		if err := json.Unmarshal(kb, &e); err != nil {
			return fmt.Errorf("%s: %w", keyNext, err)
		}
		m.KeyNext = &e
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := update.ParseManifest(body); err != nil { // what clients will read
		return err
	}
	return os.WriteFile(out, append(body, '\n'), 0o644)
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
