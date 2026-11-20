package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/project"
)

// The rules files: the user's ~/AGENTS.md, ~/CLAUDE.md and
// ~/.claude/CLAUDE.md, the workspace's root AGENTS.md and CLAUDE.md, and
// what they import (rules.go), as a kind of scoped switches (switches.go) — USER
// 2026-10-24: "the same beautiful behavior as /mcp -- global, project,
// session, mode"; 2026-10-25: one command, /rules (it was /context-file
// beside it).
//
// A file that is on is in the system prompt, as the mode says: full (copied),
// summary (the imports condensed once by the model) or index (the imports in
// raglit's index, surfaced by relevance). A file that is off is out of every
// request: the block names it, and it is in the rules index whenever the raglit
// server runs, so it is still found. The mode resolves session, project,
// global like any kind; with none of them set, the mode /rules saved in the
// project's rules.json before is used, so nothing set before is lost.

// rulesKind is the kind's name; its files keep the name they had as
// /context-file's (rules.json is the project's summary and old mode).
const (
	rulesKind      = "rules"
	rulesKindFiles = "context-files"
)

func (h *Harness) rulesSwitchKind() switchKind {
	return switchKind{
		name:  rulesKind,
		file:  rulesKindFiles,
		slash: "rules",
		modes: []string{RulesFull, RulesSummary, RulesIndex},
		items: func() []SwitchItem {
			set := h.loadRules()
			on := h.switchRules(set)
			var out []SwitchItem
			add := func(d rulesDoc, desc string) {
				it := SwitchItem{Name: d.Name, Description: desc, Source: "root", Parent: d.From}
				if d.User && d.From == "" {
					it.Source = "user"
				}
				if d.Text != "" || d.Note == "" {
					it.Tokens = h.meter.Estimate(d.heading() + "\n\n" + d.Text)
				}
				if d.From != "" {
					it.Source = "import"
				}
				if d.Added != "" {
					it.Source = "added · " + d.Added
					if d.PerWorkspace {
						it.Source += " (each project's)"
					}
				}
				if d.Note != "" {
					it.Detail = d.Note
					if d.Text == "" {
						it.Source += " · " + d.Note
					}
				}
				if n := d.includesNote(on); n != "" {
					it.Source += " · " + n
					it.Detail = strings.TrimSpace(it.Detail + "\n" + n)
				}
				out = append(out, it)
			}
			// A tree: each root, then what it imports, depth first (the
			// order they were read in), so each import sits under its file.
			kids := map[string][]rulesDoc{}
			for _, d := range set.Imports {
				kids[d.From] = append(kids[d.From], d)
			}
			var walk func(d rulesDoc)
			walk = func(d rulesDoc) {
				for _, k := range kids[d.Name] {
					add(k, "imported by "+k.From)
					walk(k)
				}
			}
			for _, d := range set.Roots {
				desc := "the project's rules (a root file: off takes the project's own rules out of every request)"
				if d.User {
					desc = "the user's rules, read in every project (off at global scope takes them out of every project's requests)"
				}
				if d.Added != "" {
					desc = "a file /rules add made a root file for " + scopeWords(d.Added) + " (/rules remove " + d.Name + " --" + d.Added + " takes it back)"
					if d.PerWorkspace {
						desc = "./" + d.Name + " (each project's): " + desc
					}
				}
				add(d, desc)
				walk(d)
			}
			return out
		},
		changed: h.applyTools,
		about: map[string]string{
			RulesFull:    "every file that is on is copied into every request",
			RulesSummary: "the root files are copied; the imports are replaced by a condensed version the model writes once",
			RulesIndex:   "the root files are copied; the imports are in raglit's index (/rag on), surfaced when they fit",
		},
		prepare: func(mode string) error {
			if mode == RulesSummary {
				return h.ensureRulesSummary(context.Background(), h.loadRules())
			}
			if mode == RulesIndex && !h.hookStarted(mcpserver.HookRulesChanged) {
				return fmt.Errorf("index mode needs raglit's index: /rag on first")
			}
			return nil
		},
		search: func() []string { return rulesSearch(h.Workspace(), !h.cfg.NoUserRules) },
		// No note: the status line says whether raglit is on, and
		// what that changes; the same thing twice was the warning (USER
		// 2026-10-25).
		status: func(string) []string {
			if h.hookStarted(mcpserver.HookRulesChanged) {
				return []string{"docs index (raglit): on · files switched off are still found by relevance"}
			}
			return []string{"docs index (raglit): off · /rag on to search files switched off, and for mode index"}
		},
	}
}

// switchRules moves the files switched off from set's roots and imports to
// set.Off.
func (h *Harness) switchRules(set rulesSet) rulesSet {
	sw := h.switchSet(rulesKind)
	keep := func(ds []rulesDoc) []rulesDoc {
		var out []rulesDoc
		for _, d := range ds {
			if on, _, _ := sw.on(d.Name); on {
				out = append(out, d)
			} else {
				set.Off = append(set.Off, d)
			}
		}
		return out
	}
	set.Roots, set.Imports = keep(set.Roots), keep(set.Imports)
	return set
}

// contextFileMode is the rules mode in force: the kind's, when a scope sets
// one, else saved (what /rules last set for the project).
func (h *Harness) contextFileMode(saved string) string {
	if m, from := h.switchSet(rulesKind).mode(RulesFull); from != "" {
		if m == RulesFull {
			return ""
		}
		return m
	}
	return saved
}

// /rules add <path> [--scope] (USER 2026-11-17): a file that is not a root
// file by name, nor imported, made one at a scope, kept in that scope's
// context-files.json "add". It is a root file like the others: in the block,
// a row in /rules, switched by its name. A path is kept relative to the
// workspace when inside it, else absolute, at session scope; absolute at
// project and global scope, whose file is read from other workspaces (a
// project's worktrees and subdirectories, every project). A relative path is
// the workspace's, ~/ is home. Except ./path at project or global scope (USER
// 2026-11-20): kept as typed, and resolved against the workspace open when
// the rules load, so `/rules add ./AGENTS.local.md --global` names each
// project's own file; its row says "(each project's)". A path without ./ is
// still absolute there. A file that is not there is a row with a note. DUN_USER_RULES=0 leaves the global adds out, with the user files.

// rulesAddPath is p as /rules add takes it: absolute, and as kept.
func (h *Harness) rulesAddPath(p string) (abs, key string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", fmt.Errorf("which file? /rules add <path>")
	}
	ws := h.Workspace()
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, _ := userHomeDir()
		if home == "" {
			return "", "", fmt.Errorf("%s: no home directory", p)
		}
		abs = filepath.Join(home, strings.TrimPrefix(p, "~"))
	case filepath.IsAbs(p):
		abs = filepath.Clean(p)
	case ws == "":
		return "", "", fmt.Errorf("%s: no workspace to be relative to", p)
	default:
		abs = filepath.Join(ws, p)
	}
	key = abs
	if ws != "" {
		if r, err := filepath.Rel(ws, abs); err == nil && r != "." && !outside(r) {
			key = r
		}
	}
	return abs, key, nil
}

// rulesAdds is the files /rules add made root files, the session's first;
// a file added at two scopes is the nearer one's.
func (h *Harness) rulesAdds() []rulesAdd {
	sw := h.switchSet(rulesKind)
	var out []rulesAdd
	seen := map[string]bool{}
	for _, sc := range switchScopes {
		for _, p := range sw[sc].Add {
			abs, _, err := h.rulesAddPath(p)
			if err != nil || seen[abs] {
				continue
			}
			seen[abs] = true
			out = append(out, rulesAdd{Path: abs, Scope: sc, PerWorkspace: perWorkspaceKey(sc, p)})
		}
	}
	return out
}

// perWorkspaceKey says key, kept at scope, is ./path to resolve per workspace.
func perWorkspaceKey(scope, key string) bool {
	return scope != mcpserver.ScopeSession && strings.HasPrefix(key, "./")
}

// listsPath says keys (a scope's "add") name the file whose real path is
// real.
func (h *Harness) listsPath(keys []string, real string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return h.addKeyIs(k, real) })
}

// addKeyIs says key names the file whose real path is real.
func (h *Harness) addKeyIs(key, real string) bool {
	a, _, err := h.rulesAddPath(key)
	return err == nil && project.RealPath(a) == real
}

// AddRule makes p a root file at scope and returns what to say. A file
// already in the block (a root file, or imported) is not added again.
func (h *Harness) AddRule(p, scope string) (string, error) {
	abs, key, err := h.rulesAddPath(p)
	if err != nil {
		return "", err
	}
	if h.switchFile(rulesKind, scope) == "" {
		return "", fmt.Errorf("there is no %s scope here", scope)
	}
	if scope != mcpserver.ScopeSession {
		// Read from other workspaces: relative would name a different file in
		// each, so absolute; except ./path, which means that (perWorkspaceKey).
		if k := filepath.ToSlash(strings.TrimSpace(p)); strings.HasPrefix(k, "./") && filepath.Clean(k) != "." {
			key = "./" + strings.TrimPrefix(filepath.Clean(k), "./")
		} else {
			key = abs
		}
	}
	set := h.loadRules()
	real := project.RealPath(abs)
	for _, d := range append(slices.Clone(set.Roots), set.Imports...) {
		// A global add is read in every project: only the user files (and
		// what they import) are in every project's block too.
		if project.RealPath(d.Path) != real || d.Added != "" || scope == mcpserver.ScopeGlobal && !d.User {
			continue
		}
		how := "a root file"
		if d.From != "" {
			how = "imported by " + d.From
		}
		return fmt.Sprintf("rules: %s is already loaded (%s): nothing to add", d.Name, how), nil
	}
	had := false
	if err := h.editSwitchFile(rulesKind, scope, func(f *SwitchFile) {
		if had = h.listsPath(f.Add, real); !had {
			f.Add = append(f.Add, key)
		}
	}); err != nil {
		return "", err
	}
	if had {
		return fmt.Sprintf("rules: %s is already added for %s", key, scopeWords(scope)), nil
	}
	h.applyTools()
	msg := fmt.Sprintf("rules: %s added for %s", key, scopeWords(scope))
	st, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		msg += " · it is not there: listed as not found until it is"
	case err != nil:
		msg += " · not readable: " + err.Error()
	default:
		msg += " (" + numfmt.HumanBytes(st.Size()) + ")"
	}
	if perWorkspaceKey(scope, key) {
		msg += " · " + key + " names each project's own file"
	}
	return msg + " · /rules remove " + key + " --" + scope + " takes it back", nil
}

// RemoveRule takes back a file /rules add made a root file at scope: p is
// its path or the name /rules shows.
func (h *Harness) RemoveRule(p, scope string) (string, error) {
	abs, key, err := h.rulesAddPath(p)
	if err != nil {
		return "", err
	}
	if h.switchFile(rulesKind, scope) == "" {
		return "", fmt.Errorf("there is no %s scope here", scope)
	}
	real, had := project.RealPath(abs), false
	if err := h.editSwitchFile(rulesKind, scope, func(f *SwitchFile) {
		had = h.listsPath(f.Add, real)
		f.Add = slices.DeleteFunc(f.Add, func(k string) bool { return h.addKeyIs(k, real) })
	}); err != nil {
		return "", err
	}
	if !had {
		return "", fmt.Errorf("%s is not added for %s (/rules lists them)", key, scopeWords(scope))
	}
	h.applyTools()
	return "rules: " + key + " taken back for " + scopeWords(scope), nil
}
