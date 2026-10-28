package main

import (
	"fmt"
	"strings"

	"github.com/iodesystems/dun"
)

// fileUsage is /file's grammar.
const fileUsage = "/file add <path> [--global|--project|--session] [--write] [--no-index] · /file remove|off <path> [scope] · " +
	"/file mode <path> read|write [scope] · /file index <path> on|off [scope] (bare: this session)"

// fileCmd runs /file <action>: a path outside the workspace, opened at a
// scope, read or write, indexed or not (dun/fileadd.go). Bare, the list.
func fileCmd(h *dun.Harness, action string) (string, error) {
	f := strings.Fields(strings.TrimSpace(action))
	if len(f) == 0 {
		return fileList(h), nil
	}
	verb := strings.ToLower(f[0])
	var words, flags []string
	write, noIndex := false, false
	for _, a := range f[1:] {
		switch strings.ToLower(a) {
		case "--write":
			write = true
		case "--no-index":
			noIndex = true
		default:
			if strings.HasPrefix(a, "--") {
				flags = append(flags, a)
			} else {
				words = append(words, a)
			}
		}
	}
	scope, err := scopeArg(flags, dun.ScopeSession)
	if err != nil {
		return "", err
	}
	// mode and index end in their value; the path is the words before it.
	value := ""
	if (verb == "mode" || verb == "index") && len(words) > 1 {
		value, words = strings.ToLower(words[len(words)-1]), words[:len(words)-1]
	}
	path := strings.Join(words, " ")
	if path == "" {
		return "", fmt.Errorf("which path? %s", fileUsage)
	}
	switch verb {
	case "add":
		return h.AddPath(path, scope, dun.PathOptions{Write: write, NoIndex: noIndex})
	case "remove", "unset":
		return h.RemovePath(path, scope)
	case "off", "on":
		return h.SetPathOn(path, scope, verb == "on")
	case "mode":
		return h.SetPathMode(path, scope, value)
	case "index":
		if value != "on" && value != "off" {
			return "", fmt.Errorf("index on or off? %s", fileUsage)
		}
		return h.SetPathIndex(path, scope, value == "on")
	}
	return "", fmt.Errorf("%s: %s", verb, fileUsage)
}

// fileList is bare /file for a client without the table.
func fileList(h *dun.Harness) string {
	es := h.AddedEntries()
	if len(es) == 0 {
		return "no paths outside the workspace are open to dun · " + fileUsage
	}
	var b strings.Builder
	b.WriteString("paths outside the workspace dun's file tools may use:")
	for _, a := range es {
		state := "off"
		if a.On {
			state = map[bool]string{true: "read and write", false: "read"}[a.Write]
		}
		if a.Dir {
			state += map[bool]string{true: " · indexed", false: " · not indexed"}[a.Index]
		}
		from := a.From
		if from == "" {
			from = "unset"
		}
		fmt.Fprintf(&b, "\n  %s — %s (%s)", a.Path, state, from)
	}
	return b.String() + "\n" + fileUsage
}
