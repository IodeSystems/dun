package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// dun's own tools in /mcp (USER 2026-10-28: "it's odd that we don't see
// system tools in /mcp and are not able to manage them"). The built-in tools
// are listed as groups, a row each ("dun:files", "dun:agents" …), each on,
// off or unset per scope like a server, and each tool in a group the same
// (/mcp tools dun:files). The nearest scope that says decides: a tool's own
// switch, then its group's; unset everywhere, on. A tool switched off is
// left out of the definitions the model is sent and refused if called.
//
// The core group — exec, ask_user, session, read_saved — is shown and
// cannot be switched off: without it a session cannot run a command, ask,
// or read back what it saved. A built-in that no group names is core.
//
// A switch changes the tools listed, which is the prompt's prefix: the
// server re-reads the conversation once. It is what the person asked for.
//
// Stored at each scope in builtins.json ($DUN_HOME, the project's state,
// <session>.builtins.json):
//
//	{"groups": {"peers": false}, "tools": {"file_write": false}}

const (
	builtinsKind = "builtins"
	// BuiltinPrefix starts a built-in group's row id in /mcp: "dun:files".
	BuiltinPrefix = "dun:"
	builtinCore   = "core"
)

type builtinGroup struct {
	name, desc string
	tools      []string // names; file_ is a prefix for the file tools
}

var builtinGroups = []builtinGroup{
	{builtinCore, "dun's core: run a command, ask you, the session's own state, read back a saved result — always on",
		[]string{"exec", "ask_user", "session", "read_saved"}},
	{"files", "read, edit and write files in the workspace and the paths /file adds", []string{"file_"}},
	{"agents", "sub-agents and background jobs", []string{"agent", "tell_parent", "jobs"}},
	{"peers", "list and message the other dun sessions (dund)", []string{"dun_list", "dun_msg"}},
	{"loops", "the session's /loop and /schedule runs", []string{"loop_ctl"}},
	{"recap", "fold a finished stretch of the turn into a recap", []string{"recap"}},
	{"memory", "notes that the project search brings back", []string{"memory"}},
	{"skills", "load a skill's instructions", []string{"skill"}},
	{"mount", "mount a directory into the docker sandbox", []string{"mount"}},
}

// groupOfTool is the group a built-in tool is in; core when none names it.
func groupOfTool(name string) string {
	for _, g := range builtinGroups {
		for _, t := range g.tools {
			if name == t || strings.HasSuffix(t, "_") && strings.HasPrefix(name, t) {
				return g.name
			}
		}
	}
	return builtinCore
}

func builtinGroupNamed(name string) (builtinGroup, bool) {
	for _, g := range builtinGroups {
		if g.name == name {
			return g, true
		}
	}
	return builtinGroup{}, false
}

type builtinsFile struct {
	Groups map[string]bool `json:"groups,omitempty"`
	Tools  map[string]bool `json:"tools,omitempty"`
}

func (h *Harness) loadBuiltins(scope string) builtinsFile {
	var f builtinsFile
	if p := h.switchFile(builtinsKind, scope); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &f)
		}
	}
	return f
}

func (h *Harness) editBuiltins(scope string, edit func(*builtinsFile)) error {
	p := h.switchFile(builtinsKind, scope)
	if p == "" {
		return fmt.Errorf("there is no %s scope here (an in-memory session)", scope)
	}
	f := h.loadBuiltins(scope)
	edit(&f)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// builtinScopes is every scope's file, nearest first.
func (h *Harness) builtinScopes() map[string]builtinsFile {
	out := map[string]builtinsFile{}
	for _, sc := range switchScopes {
		out[sc] = h.loadBuiltins(sc)
	}
	return out
}

// builtinState is a group's (tool "") or a tool's switch: whether it is on,
// the scope that decided ("" : the default, or for a tool its group), and
// each scope's word.
func builtinState(files map[string]builtinsFile, group, tool string) (on bool, from string, words map[string]string) {
	words = map[string]string{}
	pick := func(f builtinsFile) (bool, bool) {
		if tool != "" {
			v, ok := f.Tools[tool]
			return v, ok
		}
		v, ok := f.Groups[group]
		return v, ok
	}
	on, decided := true, false
	for _, sc := range switchScopes {
		if v, ok := pick(files[sc]); ok {
			words[sc] = onOffWord(v)
			if !decided {
				on, from, decided = v, sc, true
			}
		}
	}
	if !decided && tool != "" {
		on, _, _ = builtinState(files, group, "")
	}
	if group == builtinCore {
		on = true
	}
	return on, from, words
}

// builtinOff is the set of built-in tools switched off, of names.
func (h *Harness) builtinOff(names []string) map[string]bool {
	files := h.builtinScopes()
	off := map[string]bool{}
	for _, n := range names {
		if on, _, _ := builtinState(files, groupOfTool(n), n); !on {
			off[n] = true
		}
	}
	return off
}

// withBuiltinsOff drops the switched-off built-ins from defs, and refuses a
// call to one. Called by rebuildTools with the built-ins' definitions.
func (h *Harness) withBuiltinsOff(defs []llm.ToolDef, dispatch agent.ToolDispatcher) ([]llm.ToolDef, agent.ToolDispatcher) {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	h.builtinMu.Lock()
	h.builtinDefs = slices.Clone(defs)
	h.builtinMu.Unlock()
	off := h.builtinOff(names)
	if len(off) == 0 {
		return defs, dispatch
	}
	kept := defs[:0:0]
	for _, d := range defs {
		if !off[d.Function.Name] {
			kept = append(kept, d)
		}
	}
	return kept, refuseTools(dispatch, off)
}

// BuiltinGroups is a row per group for /mcp's table: on, where decided, the
// tools on and what their definitions cost.
func (h *Harness) BuiltinGroups() []ServerState {
	h.builtinMu.Lock()
	defs := slices.Clone(h.builtinDefs)
	h.builtinMu.Unlock()
	files := h.builtinScopes()
	var out []ServerState
	for _, g := range builtinGroups {
		var n, hidden, cost int
		for _, d := range defs {
			if groupOfTool(d.Function.Name) != g.name {
				continue
			}
			if on, _, _ := builtinState(files, g.name, d.Function.Name); !on {
				hidden++
				continue
			}
			n++
			cost += h.defTokens(d)
		}
		if n+hidden == 0 {
			continue // none of its tools in this session (mount without docker)
		}
		on, from, w := builtinState(files, g.name, "")
		global := w[ScopeGlobal]
		if global == "" {
			global = "on"
		}
		out = append(out, ServerState{ID: BuiltinPrefix + g.name, Running: on, Auto: on, Tools: n, Hidden: hidden, Cost: cost,
			Description: g.desc, Proto: "dun", Global: global, Project: w[ScopeProject], Session: w[ScopeSession], From: from})
	}
	return out
}

func (h *Harness) defTokens(d llm.ToolDef) int {
	b, err := json.Marshal(d)
	if err != nil {
		return 0
	}
	return h.meter.Estimate(string(b))
}

// builtinTools is ServerTools for a built-in group.
func (h *Harness) builtinTools(group string) ([]ToolInfo, error) {
	if _, ok := builtinGroupNamed(group); !ok {
		return nil, fmt.Errorf("no built-in group %q", group)
	}
	h.builtinMu.Lock()
	defs := slices.Clone(h.builtinDefs)
	h.builtinMu.Unlock()
	files := h.builtinScopes()
	var out []ToolInfo
	for _, d := range defs {
		if groupOfTool(d.Function.Name) != group {
			continue
		}
		on, from, w := builtinState(files, group, d.Function.Name)
		schema, _ := json.MarshalIndent(d.Function.Parameters, "", "  ")
		out = append(out, ToolInfo{Server: BuiltinPrefix + group, Name: d.Function.Name, Description: d.Function.Description,
			Schema: string(schema), Tokens: h.defTokens(d), Enabled: on, Global: w[ScopeGlobal], Project: w[ScopeProject],
			Session: w[ScopeSession], From: from})
	}
	return out, nil
}

// setBuiltin switches a group (tool "") or one of its tools at scope; nil
// unsets it there. The tool set is rebuilt.
func (h *Harness) setBuiltin(group, tool, scope string, on *bool) (string, error) {
	if _, ok := builtinGroupNamed(group); !ok {
		return "", fmt.Errorf("no built-in group %q — %s", group, builtinGroupList())
	}
	if group == builtinCore && on != nil && !*on {
		return "", fmt.Errorf("dun:core is always on: without it a session cannot run a command or ask you")
	}
	if !slices.Contains(switchScopes, scope) {
		return "", fmt.Errorf("scope %q: want global, project or session", scope)
	}
	err := h.editBuiltins(scope, func(f *builtinsFile) {
		m := &f.Groups
		key := group
		if tool != "" {
			m, key = &f.Tools, tool
		}
		if on == nil {
			delete(*m, key)
			return
		}
		if *m == nil {
			*m = map[string]bool{}
		}
		(*m)[key] = *on
	})
	if err != nil {
		return "", err
	}
	h.applyTools()
	return h.switchFile(builtinsKind, scope), nil
}

func builtinGroupList() string {
	names := make([]string, len(builtinGroups))
	for i, g := range builtinGroups {
		names[i] = BuiltinPrefix + g.name
	}
	return "the groups are " + strings.Join(names, ", ")
}

// builtinID splits "dun:files" into "files"; ok false for a server id.
func builtinID(id string) (string, bool) {
	return strings.CutPrefix(id, BuiltinPrefix)
}

// refuseTools answers a call to a switched-off tool with how to turn it on.
func refuseTools(inner agent.ToolDispatcher, off map[string]bool) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if n := tc.Function.Name; off[n] {
			return fmt.Sprintf("ERROR: %s is switched off in this session (/mcp, %s%s). Ask the user to turn it on if you need it.",
				n, BuiltinPrefix, groupOfTool(n)), nil
		}
		return inner(ctx, tc)
	}
}
