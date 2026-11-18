package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/proc"
)

// An extension's own slash commands (manifest slash.commands): a small
// proxy, so a manifest can give the user a command without dun knowing the
// server. /<slash> <command> <text> runs the command's steps in order:
//
//	{"tool": NAME, "args": {...}}   a call of the extension's own server,
//	                                made directly: it works for a tool in
//	                                server.disable (hidden from the model)
//	{"cli": [argv...]}              the index role's CLI (roles.index.cli),
//	                                with the serve args' store flags
//	                                (--home, --daemon, --project), detached
//
// A command is one step written inline, or "steps": [...]. A step may say
// "if": "file" or "dir": it runs with only those of the paths typed, decided
// by stat when it runs (every path must be taken by some step); and
// "unless": "--flag": it is skipped when the flag is typed (the flag is not
// part of the text); "with": "--flag" is the reverse, it runs only when the
// flag is typed. A step that errs stops the rest.
//
// Placeholders, in args' string values and in cli's elements:
//
//	{{arg}}        the text after the command, quotes removed, words joined
//	               by one space; kept as typed (case, slashes)
//	{{paths}}      each word of the text made an absolute path against the
//	               workspace (~/ is the home); a whole value only: a JSON
//	               array in args, one element per path in cli
//	{{path}}       one such path: the step runs once per path
//	{{index}}      the --index the server is started with, else the
//	               workspace's ({{index}} in server args)
//	{{project}}    the --project the server is started with, else the
//	               workspace's corpus name
//	{{workspace}}  the workspace directory
//
// The answer is each step's reply (a tool's text, JSON indented; the CLI's
// output), the step named when more than one ran.

// cmdInput is what the text after a command expands to.
type cmdInput struct {
	arg                       string
	paths                     []string
	index, project, workspace string
	flags                     map[string]bool
}

// parseCmdInput splits text (shell quoting), takes out the flags the steps
// skip on, and makes each remaining word a path.
func parseCmdInput(c mcpserver.ExtSlashCmd, text, workspace string) (cmdInput, error) {
	words, err := SplitShell(text)
	if err != nil {
		return cmdInput{}, err
	}
	known := map[string]bool{}
	for _, s := range c.AllSteps() {
		if s.Unless != "" {
			known[s.Unless] = true
		}
		if s.With != "" {
			known[s.With] = true
		}
	}
	in := cmdInput{workspace: workspace, flags: map[string]bool{}}
	var rest []string
	for _, w := range words {
		if known[w] {
			in.flags[w] = true
			continue
		}
		rest = append(rest, w)
	}
	in.arg = strings.Join(rest, " ")
	for _, w := range rest {
		in.paths = append(in.paths, absPath(w, workspace))
	}
	return in, nil
}

// absPath is p against dir: ~/ is the home, a relative path dir's.
func absPath(p, dir string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, rest)
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// expand is s with in's values; "{{paths}}" alone is paths (the caller
// places them), so it is not here.
func (in cmdInput) expand(s, path string) string {
	return strings.NewReplacer("{{arg}}", in.arg, "{{path}}", path, "{{index}}", in.index,
		"{{project}}", in.project, "{{workspace}}", in.workspace).Replace(s)
}

// expandArgs is args with the placeholders filled in, nested values too.
func (in cmdInput) expandArgs(v any, path string, paths []string) any {
	switch v := v.(type) {
	case string:
		if v == "{{paths}}" {
			out := make([]any, len(paths))
			for i, p := range paths {
				out[i] = p
			}
			return out
		}
		return in.expand(v, path)
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = in.expandArgs(x, path, paths)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = in.expandArgs(x, path, paths)
		}
		return out
	}
	return v
}

// expandCLI is argv with the placeholders filled in.
func (in cmdInput) expandCLI(argv []string, path string, paths []string) []string {
	var out []string
	for _, a := range argv {
		if a == "{{paths}}" {
			out = append(out, paths...)
			continue
		}
		out = append(out, in.expand(a, path))
	}
	return out
}

// pathKinds sorts paths by stat: "file" or "dir". A path that is neither is
// an error.
func pathKinds(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s: no such file or directory", p)
		case fi.IsDir():
			out[p] = "dir"
		case fi.Mode().IsRegular():
			out[p] = "file"
		default:
			return nil, fmt.Errorf("%s: not a file or a directory", p)
		}
	}
	return out, nil
}

// RunExtCommand carries out /<slash> <command> <text>: the command's steps,
// in order (see the top of this file). The answer is their replies.
func (h *Harness) RunExtCommand(ctx context.Context, id, command, text string) (string, error) {
	e, ok := h.extension(id)
	if !ok || e.Slash == nil {
		return "", fmt.Errorf("no extension %q with a slash command", id)
	}
	c, ok := e.Slash.Commands[command]
	if !ok {
		return "", fmt.Errorf("/%s has no command %q", e.Slash.Name, command)
	}
	usage := fmt.Sprintf("/%s %s", e.Slash.Name, command)
	if c.Help != "" {
		usage += " " + c.Help
	}
	s, err := h.knownServer(id)
	if err != nil {
		return "", err
	}
	in, err := parseCmdInput(c, text, h.cfg.Workspace)
	if err != nil {
		return "", fmt.Errorf("%v; usage: %s", err, usage)
	}
	in.index = strings.TrimSpace(argValue(s.Args, "index"))
	if in.index == "" {
		in.index = mcpserver.RaglitIndex(h.cfg.Workspace)
	}
	in.project = h.ownProject(s)

	steps := c.AllSteps()
	needsInput, byKind := false, false
	for _, st := range steps {
		needsInput = needsInput || st.TakesInput()
		byKind = byKind || st.If != ""
	}
	if needsInput && in.arg == "" {
		return "", fmt.Errorf("usage: %s", usage)
	}
	if !needsInput && in.arg != "" {
		return "", fmt.Errorf("%s takes no text; usage: %s", command, usage)
	}
	var kinds map[string]string
	if byKind {
		if kinds, err = pathKinds(in.paths); err != nil {
			return "", err
		}
	}
	// What runs: each step not skipped by a flag, with the paths it takes
	// (by kind, when it says); a step for a kind none of the paths is, not.
	type run struct {
		step  mcpserver.ExtStep
		paths []string
	}
	var runs []run
	taken := map[string]bool{}
	tools := false
	for _, st := range steps {
		if st.Unless != "" && in.flags[st.Unless] || st.With != "" && !in.flags[st.With] {
			continue
		}
		paths := in.paths
		if st.If != "" {
			paths = nil
			for _, p := range in.paths {
				if kinds[p] == st.If {
					paths = append(paths, p)
				}
			}
			if len(paths) == 0 {
				continue
			}
		}
		if st.Uses("{{path}}") || st.Uses("{{paths}}") {
			for _, p := range paths {
				taken[p] = true
			}
		}
		tools = tools || st.Tool != ""
		runs = append(runs, run{st, paths})
	}
	for _, p := range in.paths {
		if byKind && !taken[p] {
			return "", fmt.Errorf("%s is a %s, and %s takes none; usage: %s", p, kinds[p], command, usage)
		}
	}
	if tools && (h.mgr == nil || !h.mgr.ServerStarted(id)) {
		return "", fmt.Errorf("%s is not running — /%s on first", id, e.Slash.Name)
	}

	type reply struct{ what, text string }
	var replies []reply
	answer := func() string {
		if len(replies) == 1 {
			return replies[0].text
		}
		var b strings.Builder
		for i, r := range replies {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("› " + r.what + "\n" + r.text)
		}
		return b.String()
	}
	for _, rn := range runs {
		st, paths := rn.step, rn.paths
		each := []string{""}
		if st.Uses("{{path}}") {
			each = paths
		}
		for _, p := range each {
			var r reply
			var err error
			if st.Tool != "" {
				args, _ := in.expandArgs(st.Args, p, paths).(map[string]any)
				if args == nil {
					args = map[string]any{}
				}
				r.what = st.Tool
				if b, err := json.Marshal(args); err == nil && len(args) > 0 {
					r.what += " " + string(b)
				}
				r.text, err = h.mgr.CallTool(ctx, id, st.Tool, args)
				r.text = indentJSON(r.text)
			} else {
				argv := in.expandCLI(st.CLI, p, paths)
				r.what = e.Roles.Index.CLI + " " + strings.Join(argv, " ")
				r.text, err = h.runIndexCLI(ctx, e, s, argv)
			}
			if err != nil {
				replies = append(replies, reply{r.what, "error: " + err.Error()})
				return answer(), fmt.Errorf("%s: %v", r.what, err)
			}
			replies = append(replies, r)
		}
	}
	return answer(), nil
}

// indentJSON is s indented when it is a JSON object or array, else s.
func indentJSON(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return s
	}
	var b bytes.Buffer
	if json.Indent(&b, []byte(t), "", "  ") != nil {
		return s
	}
	return b.String()
}

// runIndexCLI runs e's index CLI (roles.index.cli) — the binary server s
// runs — with argv and the store flags of s's serve args, detached; the
// answer is its output.
func (h *Harness) runIndexCLI(ctx context.Context, e mcpserver.Extension, s mcpserver.Server, argv []string) (string, error) {
	if e.Roles.Index == nil || e.Roles.Index.CLI == "" {
		return "", fmt.Errorf("%s has no index CLI", e.ID)
	}
	cli := e.Roles.Index.CLI
	if filepath.Base(s.Command) != cli {
		return "", fmt.Errorf("%s runs %s, not %s: no CLI to run", e.ID, s.Command, cli)
	}
	args := append(append([]string{}, argv...), raglitStoreFlags(s.Args)...)
	out, err := proc.Detach(exec.CommandContext(ctx, s.Command, args...)).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", cli, argv[0], err, text)
	}
	return text, nil
}

// SplitShell splits a command line the way a shell would, without expanding
// anything: 'single quotes' are literal, "double quotes" take \" \\ \$ \`
// escapes, and outside quotes a backslash escapes the next character. An
// unbalanced quote or a trailing backslash is an error.
func SplitShell(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false // a word has started (so "" is an empty argument)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j == len(rs) {
				return nil, fmt.Errorf("unbalanced ' quote")
			}
			cur.WriteString(string(rs[i+1 : j]))
			i, in = j, true
		case r == '"':
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[j+1]) {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j == len(rs) {
				return nil, fmt.Errorf("unbalanced \" quote")
			}
			i, in = j, true
		case r == '\\':
			if i+1 == len(rs) {
				return nil, fmt.Errorf("a trailing \\ escapes nothing")
			}
			i++
			cur.WriteRune(rs[i])
			in = true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}
