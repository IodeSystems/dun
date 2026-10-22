package main

import (
	"encoding/json"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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

// allSlash is the static commands plus one per MCP prompt.
func (m tuiModel) allSlash() []slashCmd {
	if len(m.prompts) == 0 {
		return slashCommands
	}
	out := append([]slashCmd(nil), slashCommands...)
	for _, p := range m.prompts {
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
		out = append(out, slashCmd{p.Server + ":" + p.Name, strings.Join(args, " "), desc, promptSlash(p)})
	}
	return out
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
	if len(args) == 0 {
		if len(m.skills) == 0 {
			m.append(stDim.Render("no skills — add <name>/SKILL.md under ~/.dun/skills or .dun/skills (Claude Code's ~/.claude/skills is read too)"))
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
		m.append(stErr.Render("no skill " + name + " — /skill lists them"))
		return nil
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
