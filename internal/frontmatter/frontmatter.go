// Package frontmatter reads the leading --- block of a markdown file:
// SKILL.md files and memory notes. A leaf, so harness and memory share it.
package frontmatter

import (
	"encoding/json"
	"strings"
)

// Split returns the top-level scalar keys of a leading ---
// block, and the body after it. It reads the subset SKILL.md files use:
// key: value, quoted values, and | or > blocks for a long description.
func Split(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return meta, text
	}
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, text
	}
	block, body := rest[:end], rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "|" || v == ">" || v == "|-" || v == ">-" {
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			v = strings.TrimSpace(strings.Join(parts, " "))
		} else if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			if v[0] == '"' {
				var u string
				if json.Unmarshal([]byte(v), &u) == nil {
					v = u
				} else {
					v = v[1 : len(v)-1]
				}
			} else {
				v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
			}
		}
		meta[k] = v
	}
	return meta, body
}
