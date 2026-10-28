package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The engine's catalog event: the skills in reach and the running MCP
// servers' prompts. Skills are offered through /skill; each prompt becomes a
// slash command of its own, /<server>:<prompt>, with its arguments taken in
// the order the server declares them (the last one takes the rest of the
// line).

type catalogSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	UserOnly    bool   `json:"userOnly"`
}

type catalogPrompt struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Arguments   []struct {
		Name     string `json:"name"`
		Required bool   `json:"required"`
	} `json:"arguments"`
}

// noteCatalog takes a catalog event in.
func (m *tuiModel) noteCatalog(ev evMsg) {
	b, _ := json.Marshal(map[string]any{"skills": ev["skills"], "prompts": ev["prompts"]})
	var c struct {
		Skills  []catalogSkill  `json:"skills"`
		Prompts []catalogPrompt `json:"prompts"`
	}
	_ = json.Unmarshal(b, &c)
	m.skills, m.prompts = c.Skills, c.Prompts
}

// Commands are listed by group, in this order: a new user's first (setting
// up: logging in, the model, servers, help), dun's own maintenance
// last. Within a group, the order they are defined in. An
// engine command carries its group (proto.CommandSpec.Group); the TUI's own
// are in tuiGroup. MCP prompts are "prompts".
var groupOrder = []string{"setup", "tools", "session", "git", "memory", "prompts", "client", "maintenance"}

var tuiGroup = map[string]string{
	"login": "setup", "model": "setup", "mcp add": "setup", "help": "setup", "config": "setup", "logout": "setup",
	"skill":  "tools",
	"status": "session", "context": "session", "resume": "session", "new": "session", "persist": "session", "exit": "session", "close": "session", "splash": "session",
	"reconnect": "client", "mouse": "client",
	"reload": "maintenance", "upgrade": "maintenance", "version": "maintenance", "perf": "maintenance", "repaint": "maintenance",
	"scrolldebug": "maintenance", "trace": "maintenance",
}

// debugCommands are for working on dun itself: /help and the / palette leave
// them out (/help --debug lists them), and they run when typed by their full
// name — a prefix of one is not taken for it. Deciding: does a person using
// dun on their project ever need it? A redraw timing, a scroll dump, an event
// trace, a bug-report capture and a launcher rebuild do not.
var debugCommands = map[string]bool{"perf": true, "repaint": true, "scrolldebug": true, "trace": true}

func groupRank(g string) int {
	if i := slices.Index(groupOrder, g); i >= 0 {
		return i
	}
	return slices.Index(groupOrder, "client") // an unknown group: before maintenance
}

// allSlash is the engine's commands, the TUI's, and one per MCP prompt, in
// group order (within a group, the engine's first).
func (m tuiModel) allSlash() []slashCmd {
	type ranked struct {
		c    slashCmd
		rank int
	}
	var rs []ranked
	for _, c := range m.engineCmds {
		rs = append(rs, ranked{slashCmd{c.name, c.usage, c.desc, engineSlash(c.name)}, groupRank(c.group)})
	}
	for _, c := range slashCommands {
		rs = append(rs, ranked{c, groupRank(tuiGroup[c.name])})
	}
	for _, p := range m.prompts {
		rs = append(rs, ranked{m.promptCmd(p), groupRank("prompts")})
	}
	slices.SortStableFunc(rs, func(a, b ranked) int { return a.rank - b.rank })
	out := make([]slashCmd, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.c)
	}
	return out
}

// promptCmd is an MCP prompt as a slash command.
func (m tuiModel) promptCmd(p catalogPrompt) slashCmd {
	var args []string
	for _, a := range p.Arguments {
		if a.Required {
			args = append(args, "<"+a.Name+">")
		} else {
			args = append(args, "["+a.Name+"]")
		}
	}
	desc := p.Description
	if desc == "" {
		desc = "prompt from MCP server " + p.Server
	}
	return slashCmd{p.Server + ":" + p.Name, strings.Join(args, " "), desc, promptSlash(p)}
}

// promptSlash asks the engine to render the prompt; the text comes back as
// a prompt event and is sent as the user's message.
func promptSlash(p catalogPrompt) func(*tuiModel, []string) tea.Cmd {
	return func(m *tuiModel, words []string) tea.Cmd {
		args := map[string]string{}
		for i, a := range p.Arguments {
			if i >= len(words) {
				if a.Required {
					m.append(stErr.Render("/" + p.Server + ":" + p.Name + " needs " + a.Name))
					return nil
				}
				continue
			}
			if i == len(p.Arguments)-1 {
				args[a.Name] = strings.Join(words[i:], " ")
			} else {
				args[a.Name] = words[i]
			}
		}
		b, _ := json.Marshal(map[string]any{"server": p.Server, "name": p.Name, "args": args})
		if !m.proc.controlCmd("prompt", string(b)) {
			m.append(stErr.Render("no engine right now — /reconnect first"))
			return nil
		}
		m.append(stDim.Render("getting prompt " + p.Server + ":" + p.Name + "…"))
		return nil
	}
}

// skillSlash is /skill: bare lists the skills; /skill <name> [text] asks the
// model to use that skill, for the text when there is one.
func skillSlash(m *tuiModel, args []string) tea.Cmd {
	// /skill info is the list, as the splash's skills row says — unless a
	// skill is named info.
	if len(args) == 1 && args[0] == "info" && !slices.ContainsFunc(m.skills, func(s catalogSkill) bool { return s.Name == "info" }) {
		args = nil
	}
	// The switches (switchlist.go): bare opens the list; on|off|unset with a
	// name or glob, or mode with a mode, does it; one that is missing what
	// it needs opens the list (docs/ui-decisions.md). A skill named like a
	// verb is still run by /skill <name> with arguments after it.
	if len(args) > 0 && skillVerb(m.skills, args[0]) {
		if len(args) >= 2 { // a name or glob, or a mode: complete
			if !m.proc.controlCmd("switches", "skills "+strings.Join(args, " ")) {
				m.append(stErr.Render("no engine right now — /reconnect first"))
			}
			return nil
		}
		args = nil
	}
	if len(args) == 0 && len(m.skills) > 0 {
		m.openSwitches("skills")
		return nil
	}
	if len(args) == 0 {
		if len(m.skills) == 0 {
			m.append(stDim.Render("no skills — add <name>/SKILL.md under ~/.dun/skills or .dun/skills (Claude Code's ~/.claude/skills is read too)"))
			return nil
		}
		if len(m.skills) > skillListAll {
			m.append(skillSummary(m.skills))
			return nil
		}
		var b strings.Builder
		b.WriteString(stHeader.Render("skills"))
		for _, s := range m.skills {
			line := "\n  " + stTool.Render(s.Name) + stDim.Render("  "+s.Source)
			if s.UserOnly {
				line += stDim.Render(" · /skill only")
			}
			b.WriteString(line + "\n    " + stDim.Render(s.Description))
		}
		if tok := m.skillTokens(); tok > 0 {
			b.WriteString("\n" + stDim.Render("the list costs ~"+tokN(tok)+" tokens in every request; a skill's body is read only when it is used"))
		}
		m.append(b.String())
		return nil
	}
	name := args[0]
	found := false
	for _, s := range m.skills {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		// Not a name: a part of one. One skill's name matches → that skill;
		// more → list them; none by name → list what the descriptions match.
		all := skillChoices(m, name).choices
		var byName []argChoice
		for _, c := range all {
			if c.hits != nil { // matched on the name, not the description

				byName = append(byName, c)
			}
		}
		switch {
		case len(byName) == 1:
			name = byName[0].value
		case len(all) == 0:
			m.append(stErr.Render("no skill matches " + name + " — /skill lists them"))
			return nil
		default:
			m.append(skillMatches(name, all, len(m.skills), max(40, m.w-4)))
			return nil
		}
	}
	text := "Use the skill " + name + " (call skill name=" + name + " first)"
	if len(args) > 1 {
		text += ": " + strings.Join(args[1:], " ")
	} else {
		text += "."
	}
	*m = m.sendUser(text)
	return nil
}

// skillListAll is how many skills bare /skill lists one by one; more are
// summarised by source.
const skillListAll = 15

// skillMatchMax is how many matches /skill <part of a name> lists.
const skillMatchMax = 20

// skillSummary is bare /skill for many skills: a count per source, in the
// order the sources first appear, and how to find one.
func skillSummary(skills []catalogSkill) string {
	var order []string
	count := map[string]int{}
	for _, s := range skills {
		if count[s.Source] == 0 {
			order = append(order, s.Source)
		}
		count[s.Source]++
	}
	w := 0
	for _, src := range order {
		w = max(w, len(src))
	}
	var b strings.Builder
	b.WriteString(stHeader.Render(commaf("%d skills", len(skills))))
	for _, src := range order {
		b.WriteString("\n  " + stDim.Render(fmt.Sprintf("%-*s", w, src)) + "  " + stTool.Render(commaf("%d", count[src])))
	}
	b.WriteString("\n" + stDim.Render("type /skill <part of a name> to search — the palette lists the matches as you type"))
	return b.String()
}

// skillMatches lists the skills a query matched, best first, each on one
// row of width w.
func skillMatches(query string, ms []argChoice, total, w int) string {
	var b strings.Builder
	b.WriteString(stHeader.Render(fmt.Sprintf("%d of %d skills match %q", len(ms), total, query)))
	for i, c := range ms {
		if i == skillMatchMax {
			b.WriteString("\n  " + stDim.Render(fmt.Sprintf("… %d more — type more of the name", len(ms)-i)))
			break
		}
		b.WriteString("\n  " + choiceLine(c, w))
	}
	b.WriteString("\n" + stDim.Render("/skill <name> [text] runs one"))
	return b.String()
}

// engineCmd is one of the engine's slash commands (proto.CommandSpec): the
// TUI shows and completes it, and the engine carries it out.
type engineCmd struct {
	name, usage, desc string
	help              string         // the command in full, for /help <name>
	group             string         // where it is listed (groupOrder)
	choices           []engineChoice // the first argument's values
}

type engineChoice struct{ value, desc string }

func engineCmdsFromEvent(ev evMsg) []engineCmd {
	items, _ := ev["items"].([]any)
	out := make([]engineCmd, 0, len(items))
	for _, it := range items {
		c, _ := it.(map[string]any)
		if n := str(c["name"]); n != "" {
			ec := engineCmd{name: n, usage: str(c["usage"]), desc: str(c["description"]), help: str(c["help"]), group: str(c["group"])}
			chs, _ := c["choices"].([]any)
			for _, ch := range chs {
				if cm, ok := ch.(map[string]any); ok && str(cm["value"]) != "" {
					ec.choices = append(ec.choices, engineChoice{str(cm["value"]), str(cm["description"])})
				}
			}
			out = append(out, ec)
		}
	}
	return out
}

// engineSlash sends a slash command to the engine as the line typed; the
// engine routes it (cmd/dun commands.go: routeSlash) exactly as it does for
// every other client, so the TUI cannot drift from them. Adding an MCP server
// is the one exception: it is a form only this TUI has.
func engineSlash(name string) func(m *tuiModel, args []string) tea.Cmd {
	return func(m *tuiModel, args []string) tea.Cmd {
		if name == "mcp" && len(args) > 0 && args[0] == "add" {
			return m.openMCPForm(args[1:]...)
		}
		if name == "rag" && len(args) == 1 && strings.EqualFold(args[0], "setup") {
			return m.openRagSetup()
		}
		// /interrupt: the same command ctrl+g sends, so the status line says
		// "interrupting…" for it too.
		if name == "interrupt" && m.turnRunning() {
			nm, cmd := m.interrupt()
			*m = nm.(tuiModel)
			return cmd
		}
		// /jobs kill all asks first: it ends every running job at once.
		if name == "jobs" && len(args) == 2 && strings.EqualFold(args[0], "kill") && strings.EqualFold(args[1], "all") {
			m.askConfirm("kill every running job? (y/n)", func(m *tuiModel) { m.killJobsCmd("jobs kill all") })
			return nil
		}
		// /mcp tools <id> goes to the engine like any other; the list it
		// answers with opens in the pager because this TUI asked for it.
		if name == "mcp" && len(args) == 2 && args[0] == "tools" {
			m.openMCPTools(args[1])
		}
		// Bare /rules, or an operation missing what it works on: the list
		// (switchlist.go, docs/ui-decisions.md).
		if name == "rules" && (len(args) == 0 || len(args) == 1 && slices.Contains([]string{"on", "off", "unset", "mode"}, args[0])) {
			m.openSwitches("rules")
			return nil
		}
		// Bare /rag attach: every index, to search and attach at a scope
		// (dun/indexkind.go; USER 2026-10-27/28: "a /mcp like interface for
		// /rag"). Bare /rag is it; /rag status is the capability report.
		if name == "rag" && (len(args) == 0 || len(args) == 1 && args[0] == "attach") {
			m.openSwitches("indexes")
			return nil
		}
		// Bare /file: the paths outside the workspace, as a table like
		// /mcp's (dun/fileadd.go; USER 2026-10-28).
		if name == "file" && len(args) == 0 {
			m.openSwitches("paths")
			return nil
		}
		// Bare /effort and /retry: the picker, which is the palette's choices
		// for it (USER 2026-10-26: empty shows the modes).
		if (name == "effort" || name == "retry") && len(args) == 0 {
			m.input.SetValue("/" + name + " ")
			m.input.CursorEnd()
			m.paletteSel = 0
			return nil
		}
		// Bare /ext: the list, in its screen (extview.go).
		if name == "ext" && len(args) == 0 {
			m.openExtensions()
			return nil
		}
		// Bare /mcp: the listing opens as the switch table (mcpview.go).
		if name == "mcp" && (len(args) == 0 || len(args) == 1 && (args[0] == "list" || args[0] == "status")) {
			m.openMCPServers()
		}
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		// /schedule and /loop carry a message: the rest of the line as typed.
		if name == "schedule" || name == "loop" {
			_, rest := cutFields(strings.TrimPrefix(strings.TrimSpace(m.slashLine), "/"), 1)
			line = name + " " + rest
			if len(args) == 2 && strings.EqualFold(args[0], "edit") {
				m.schedEditAsk = strings.TrimPrefix(args[1], "#")
			}
		}
		if !m.proc.controlCmd("slash", line) {
			m.append(stErr.Render("no engine right now — /reconnect first"))
		}
		return nil
	}
}

// skillVerb reports whether /skill's arguments are a switch operation, not a
// skill to run: on, off, unset or mode, and no skill of that name.
func skillVerb(skills []catalogSkill, a string) bool {
	switch a {
	case "on", "off", "unset", "mode":
		return !slices.ContainsFunc(skills, func(s catalogSkill) bool { return s.Name == a })
	}
	return false
}
