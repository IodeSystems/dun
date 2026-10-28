package dun

import (
	"context"
	"fmt"
)

// The rules files: the workspace's root AGENTS.md and CLAUDE.md and what
// they import (rules.go), as a kind of scoped switches (switches.go) — USER
// 2026-10-24: "the same beautiful behavior as /mcp -- global, project,
// session, mode"; 2026-10-25: one command, /rules (it was /context-file
// beside it).
//
// A file that is on is in the system prompt, as the mode says: full (copied),
// summary (the imports condensed once by the model) or index (the imports in
// the docs index, surfaced by relevance). A file that is off is out of every
// request: the block names it, and it is in the rules index whenever the docs
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
			set := loadRules(h.cfg.Workspace)
			var out []SwitchItem
			add := func(d rulesDoc, desc string) {
				it := SwitchItem{Name: d.Name, Description: desc, Source: "root", Tokens: h.meter.Estimate(d.heading() + "\n\n" + d.Text)}
				if d.From != "" {
					it.Source = "import"
				}
				if d.Note != "" {
					it.Detail = d.Note
				}
				out = append(out, it)
			}
			for _, d := range set.Roots {
				add(d, "the project's rules (a root file: off takes the project's own rules out of every request)")
			}
			for _, d := range set.Imports {
				add(d, "imported by "+d.From)
			}
			return out
		},
		changed: h.applyTools,
		about: map[string]string{
			RulesFull:    "every file that is on is copied into every request",
			RulesSummary: "the root files are copied; the imports are replaced by a condensed version the model writes once",
			RulesIndex:   "the root files are copied; the imports are in the docs index, surfaced when they fit",
		},
		prepare: func(mode string) error {
			if mode == RulesSummary {
				return h.ensureRulesSummary(context.Background(), loadRules(h.cfg.Workspace))
			}
			if mode == RulesIndex && !h.hookStarted(HookRulesChanged) {
				return fmt.Errorf("index mode needs the docs index: /rag on first")
			}
			return nil
		},
		// No note: the status line says whether the docs index is on, and
		// what that changes; the same thing twice was the warning (USER
		// 2026-10-25).
		status: func(string) []string {
			if h.hookStarted(HookRulesChanged) {
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
