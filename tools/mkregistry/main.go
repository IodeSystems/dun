// mkregistry writes the registry's index.json (docs/registry.md) from the
// entries under -src (site/registry/v1): one JSON file per entry, in
// extensions/, presets/ and tools/. An entry is the index's entry, with two
// conveniences resolved here and inlined:
//
//	"manifest_file": "examples/extensions/raglit/extension.json"   (an extension's manifest, a path from -root)
//	"files_from": "release/deps.json"                              (a tool's downloads at its recommended version)
//
// Every entry is checked as the dun this is built from reads it: the index
// parses (ranges, versions) and an extension's manifest loads. It signs
// nothing: the site workflow signs the output with minisign.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/registry"
	"github.com/iodesystems/dun/internal/update"
)

func main() {
	src := flag.String("src", "site/registry/v1", "the entries")
	root := flag.String("root", ".", "where manifest_file and files_from paths start")
	out := flag.String("o", "", "the index to write (stdout when empty)")
	published := flag.String("published", "", "the publication time, RFC 3339 (default: now)")
	flag.Parse()
	when := time.Now().UTC()
	if *published != "" {
		t, err := time.Parse(time.RFC3339, *published)
		if err != nil {
			fail(err)
		}
		when = t
	}
	b, err := build(*src, *root, when)
	if err != nil {
		fail(err)
	}
	if *out == "" {
		os.Stdout.Write(b)
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkregistry:", err)
	os.Exit(1)
}

// source is an entry as written: the index's fields and the conveniences.
type source struct {
	ManifestFile string `json:"manifest_file,omitempty"`
	FilesFrom    string `json:"files_from,omitempty"`
}

// build reads every entry under src and returns the index, checked.
func build(src, root string, published time.Time) ([]byte, error) {
	ix := registry.Index{Schema: registry.Schema, Published: published.Format(time.RFC3339)}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") || filepath.Base(p) == "index.json" {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		kind, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var conv source
		if err := json.Unmarshal(b, &conv); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		b, err = stripConveniences(b)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		switch kind {
		case "extensions":
			var e registry.ExtEntry
			if err := strict(b, &e); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if conv.ManifestFile != "" {
				m, err := os.ReadFile(filepath.Join(root, conv.ManifestFile))
				if err != nil {
					return fmt.Errorf("%s: manifest_file: %w", p, err)
				}
				e.Manifest = m
			}
			ext, err := dun.ParseExtension(e.Manifest)
			if err != nil {
				return fmt.Errorf("%s: the manifest does not load: %w", p, err)
			}
			if ext.ID != e.ID {
				return fmt.Errorf("%s: the manifest's id %q is not the entry's %q", p, ext.ID, e.ID)
			}
			ix.Extensions = append(ix.Extensions, e)
		case "presets":
			var pr registry.Preset
			if err := strict(b, &pr); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			ix.Presets = append(ix.Presets, pr)
		case "tools":
			var t registry.ToolEntry
			if err := strict(b, &t); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if conv.FilesFrom != "" {
				f, err := filesFrom(filepath.Join(root, conv.FilesFrom), t.ID, t.Recommended)
				if err != nil {
					return fmt.Errorf("%s: files_from: %w", p, err)
				}
				t.Files = f
			}
			ix.Tools = append(ix.Tools, t)
		default:
			return fmt.Errorf("%s: not in extensions/, presets/ or tools/", p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortEntries(ix.Extensions, func(e registry.ExtEntry) registry.Entry { return e.Entry })
	sortEntries(ix.Presets, func(e registry.Preset) registry.Entry { return e.Entry })
	sortEntries(ix.Tools, func(e registry.ToolEntry) registry.Entry { return e.Entry })
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // ranges stay readable: >=0.1.3, not \u003e=0.1.3
	enc.SetIndent("", "  ")
	if err := enc.Encode(ix); err != nil {
		return nil, err
	}
	b := []byte(buf.String())
	if _, err := registry.Parse(b); err != nil { // as dun reads it
		return nil, err
	}
	return b, nil
}

// stripConveniences is an entry without manifest_file and files_from, for
// the strict decode.
func stripConveniences(b []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	delete(m, "manifest_file")
	delete(m, "files_from")
	return json.Marshal(m)
}

func strict(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// filesFrom is tool's downloads in a release deps.json, which must name
// version.
func filesFrom(path, tool, version string) (map[string]update.File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := update.ParseManifest(b)
	if err != nil {
		return nil, err
	}
	a, ok := m.Artifacts[tool]
	if !ok {
		return nil, fmt.Errorf("%s has no %s", path, tool)
	}
	if a.Version != version {
		return nil, fmt.Errorf("%s has %s %s, the entry recommends %s", path, tool, a.Version, version)
	}
	if len(a.Files) == 0 {
		return nil, errors.New("no files")
	}
	return a.Files, nil
}

// sortEntries orders entries by id, then version.
func sortEntries[T any](s []T, entry func(T) registry.Entry) {
	sort.SliceStable(s, func(i, j int) bool {
		a, b := entry(s[i]), entry(s[j])
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		va, ea := semver.NewVersion(a.Version)
		vb, eb := semver.NewVersion(b.Version)
		if ea != nil || eb != nil {
			return a.Version < b.Version
		}
		return va.LessThan(vb)
	})
}
