package mcpserver

import (
	"maps"
	"slices"
	"strings"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// The sections of dun.json beside its servers: how /worktree commit writes
// its message (commit.go), the file tools (files.go) and what dund's indexes
// are given (rag_ingest.go), in internal/harness. They are read across the
// same layers as the servers.

// CommitConfig is the `commit` section of dun.json: how the message is written.
type CommitConfig struct {
	// Format names a built-in style. "conventional" (the default) or "plain".
	Format string `json:"format,omitempty"`
	// Instruction replaces the built-in rules with free text handed to the
	// model verbatim, for a project whose convention is its own. It wins over
	// Format, which is why both can be set without a precedence puzzle: the
	// specific statement beats the named one.
	Instruction string `json:"instruction,omitempty"`
}

// FilesConfig is the "files" section of dun.json.
type FilesConfig struct {
	// Tools enables tools by name: read, edit, write, grep, list, delete,
	// move. nil inherits (default: DefaultFileTools); an empty list turns the
	// file tools off.
	// No omitempty: null inherits and [] is off, and both must round-trip.
	Tools []string `json:"tools"`
	// ReadOnly makes the workspace read only: the tools that change files stay
	// listed and refuse (harness withFiles). A pointer so a higher layer can
	// say false.
	ReadOnly *bool `json:"read_only,omitempty"`
	// Caps; 0 inherits agentkit's defaults (64KB ranged read, 8KB unranged,
	// 100 grep matches, 200 list entries).
	ReadMax     int `json:"read_max,omitempty"`
	UnrangedMax int `json:"unranged_max,omitempty"`
	GrepMax     int `json:"grep_max,omitempty"`
	ListMax     int `json:"list_max,omitempty"`
	// Ignore are gitignore-syntax patterns hidden from grep and list on top
	// of .gitignore. Accumulated across layers, not replaced.
	Ignore []string `json:"ignore,omitempty"`
}

// LoadFiles resolves the effective files config for a workspace across the
// same layers as LoadCommit. nil means no section anywhere: the defaults.
func LoadFiles(dir string) *FilesConfig {
	var out *FilesConfig
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := ReadConfig(path)
		if err != nil || f == nil || f.Files == nil {
			continue
		}
		if out == nil {
			cp := *f.Files
			out = &cp
			continue
		}
		merged := mergeFiles(*out, *f.Files)
		out = &merged
	}
	return out
}

// mergeFiles layers next over prev: what next states wins, what it does not
// state is inherited, and ignore patterns add up.
func mergeFiles(prev, next FilesConfig) FilesConfig {
	if next.Tools != nil {
		prev.Tools = next.Tools
	}
	if next.ReadOnly != nil {
		prev.ReadOnly = next.ReadOnly
	}
	if next.ReadMax != 0 {
		prev.ReadMax = next.ReadMax
	}
	if next.UnrangedMax != 0 {
		prev.UnrangedMax = next.UnrangedMax
	}
	if next.GrepMax != 0 {
		prev.GrepMax = next.GrepMax
	}
	if next.ListMax != 0 {
		prev.ListMax = next.ListMax
	}
	if len(next.Ignore) > 0 {
		prev.Ignore = append(append([]string(nil), prev.Ignore...), next.Ignore...)
	}
	return prev
}

// RagConfig is the "rag" section of dun.json.
type RagConfig struct {
	Include     []string `json:"include,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
	MaxFileKB   int      `json:"max_file_kb,omitempty"`
	MaxDirFiles int      `json:"max_dir_files,omitempty"`
	MaxDirMB    int      `json:"max_dir_mb,omitempty"`
}

// LoadRag resolves the effective rag config across the same layers as
// LoadFiles. nil means no section anywhere: the defaults.
func LoadRag(dir string) *RagConfig {
	var out *RagConfig
	for _, p := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := ReadConfig(p)
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

// RepoTier is a tier in dun.json. Only Use is honored.
type RepoTier struct {
	Use      string `json:"use,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// LoadTiers is what a workspace's dun.json layers say of the model tiers:
// use texts and default_tier, the later layer winning. $DUN_HOME/mcp.json is
// not read: the tiers' home is $DUN_HOME/config.json. A provider, model or
// effort set here is named in Ignored and has no effect.
func LoadTiers(dir string) dunconfig.RepoTiers {
	var out dunconfig.RepoTiers
	for _, p := range serverConfigPaths(dir) {
		f, err := ReadConfig(p)
		if err != nil || f == nil {
			continue
		}
		if s := strings.TrimSpace(f.DefaultTier); s != "" {
			out.Default = s
		}
		for _, name := range slices.Sorted(maps.Keys(f.Tiers)) {
			t := f.Tiers[name]
			if s := strings.TrimSpace(t.Use); s != "" {
				if out.Use == nil {
					out.Use = map[string]string{}
				}
				out.Use[name] = s
			}
			for _, x := range []struct{ field, v string }{{"provider", t.Provider}, {"model", t.Model}, {"effort", t.Effort}} {
				if strings.TrimSpace(x.v) != "" {
					out.Ignored = append(out.Ignored, "tiers."+name+"."+x.field+" ("+p+")")
				}
			}
		}
	}
	return out
}
