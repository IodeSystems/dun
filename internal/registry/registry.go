// Package registry reads dun's registry (docs/registry.md): extension
// manifests, MCP presets and tool pins published on dun.iodesystems.com, so
// a change to one of them is not a dun release. One signed file holds
// every entry; each entry says, in npm's range syntax, which dun versions
// read it ("dun") and, for an extension, which versions of its tool it
// works with ("tools"). The newest entry that fits wins; with none, dun
// uses what it was built with.
package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/iodesystems/dun/internal/update"
)

// Schema is the index format this build reads.
const Schema = 1

// MaxIndex is the most of an index read from the network.
const MaxIndex = 1 << 20

// Index is the registry: every entry, all versions.
type Index struct {
	Schema     int         `json:"schema"`
	Published  string      `json:"published"`
	Extensions []ExtEntry  `json:"extensions,omitempty"`
	Presets    []Preset    `json:"presets,omitempty"`
	Tools      []ToolEntry `json:"tools,omitempty"`
}

// Entry is what every kind of entry has: an id, its own version, and the
// dun versions that read it.
type Entry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Dun     string `json:"dun"` // npm range: the dun versions that read this entry
}

// ExtEntry is an extension manifest for a range of its tool's versions.
type ExtEntry struct {
	Entry
	Tools    string          `json:"tools"`    // npm range: the tool versions it works with
	Manifest json.RawMessage `json:"manifest"` // an extension.json, as dun reads it
}

// Preset is an MCP server /mcp add suggests.
type Preset struct {
	Entry
	Description string   `json:"description"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
}

// ToolEntry pins a tool: the oldest version this dun works with, the one
// to install, and that one's downloads.
type ToolEntry struct {
	Entry
	Min         string                 `json:"min"`
	Recommended string                 `json:"recommended"`
	Files       map[string]update.File `json:"files,omitempty"`
}

// Parse reads an index and checks every entry: its versions are semver and
// its ranges parse. An index with one bad entry is refused whole: it was
// published by mistake.
func Parse(b []byte) (*Index, error) {
	var ix Index
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	if ix.Schema != Schema {
		return nil, fmt.Errorf("registry: schema %d, this build reads %d", ix.Schema, Schema)
	}
	check := func(kind string, e Entry, ranges ...string) error {
		if e.ID == "" {
			return fmt.Errorf("registry: a %s entry with no id", kind)
		}
		if _, err := semver.StrictNewVersion(e.Version); err != nil {
			return fmt.Errorf("registry: %s %s: version %q: %v", kind, e.ID, e.Version, err)
		}
		for _, r := range append([]string{e.Dun}, ranges...) {
			if _, err := semver.NewConstraint(r); err != nil {
				return fmt.Errorf("registry: %s %s %s: range %q: %v", kind, e.ID, e.Version, r, err)
			}
		}
		return nil
	}
	for _, e := range ix.Extensions {
		if err := check("extension", e.Entry, e.Tools); err != nil {
			return nil, err
		}
		if len(e.Manifest) == 0 {
			return nil, fmt.Errorf("registry: extension %s %s has no manifest", e.ID, e.Version)
		}
	}
	for _, p := range ix.Presets {
		if err := check("preset", p.Entry); err != nil {
			return nil, err
		}
		if (p.Command == "") == (p.URL == "") {
			return nil, fmt.Errorf("registry: preset %s: a command or a url, not both", p.ID)
		}
	}
	for _, t := range ix.Tools {
		if err := check("tool", t.Entry); err != nil {
			return nil, err
		}
		for _, v := range []string{t.Min, t.Recommended} {
			if _, err := semver.NewVersion(v); err != nil {
				return nil, fmt.Errorf("registry: tool %s: %q is not a version", t.ID, v)
			}
		}
	}
	return &ix, nil
}

// Verify checks body's signature with keys, then parses it.
func Verify(body []byte, sig string, keys []update.PublicKey) (*Index, error) {
	if _, err := update.VerifyAny(body, sig, keys); err != nil {
		return nil, fmt.Errorf("registry: signature: %w", err)
	}
	return Parse(body)
}

// PublishedAt is the index's publication time; zero when it has none.
func (ix *Index) PublishedAt() time.Time {
	t, _ := time.Parse(time.RFC3339, ix.Published)
	return t
}

// fits reports whether version v is in npm range r. An empty or
// unparsable v (a checkout's "devel", a dev dun) fits any range: it says
// nothing to rule an entry out.
func fits(r, v string) bool {
	c, err := semver.NewConstraint(r)
	if err != nil {
		return false
	}
	sv, err := semver.NewVersion(strings.TrimPrefix(v, "v"))
	if err != nil {
		return true
	}
	// A pre-release (v0.2.0-rc.1) or a pseudo-version (a pushed commit)
	// is compared as its release: npm leaves pre-releases out of a range
	// unless it names one, which would leave every dev tool out.
	if sv.Prerelease() != "" {
		rel, _ := sv.SetPrerelease("")
		sv = &rel
	}
	return c.Check(sv)
}

func newer(a, b string) bool {
	va, ea := semver.NewVersion(a)
	vb, eb := semver.NewVersion(b)
	return ea == nil && (eb != nil || va.GreaterThan(vb))
}

// Extension is the newest entry for extension id that dun version dunV
// reads and that works with the tool at toolV; false when none does.
func (ix *Index) Extension(id, dunV, toolV string) (ExtEntry, bool) {
	var best ExtEntry
	found := false
	for _, e := range ix.Extensions {
		if e.ID == id && fits(e.Dun, dunV) && fits(e.Tools, toolV) && (!found || newer(e.Version, best.Version)) {
			best, found = e, true
		}
	}
	return best, found
}

// PresetsFor are the presets dun version dunV reads, the newest of each id,
// in the index's order.
func (ix *Index) PresetsFor(dunV string) []Preset {
	at := map[string]int{}
	var out []Preset
	for _, p := range ix.Presets {
		if !fits(p.Dun, dunV) {
			continue
		}
		if i, ok := at[p.ID]; ok {
			if newer(p.Version, out[i].Version) {
				out[i] = p
			}
			continue
		}
		at[p.ID] = len(out)
		out = append(out, p)
	}
	return out
}

// Tool is the newest pin for tool name that dun version dunV reads.
func (ix *Index) Tool(name, dunV string) (ToolEntry, bool) {
	var best ToolEntry
	found := false
	for _, t := range ix.Tools {
		if t.ID == name && fits(t.Dun, dunV) && (!found || newer(t.Version, best.Version)) {
			best, found = t, true
		}
	}
	return best, found
}
