package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// The "lsp" switch kind (USER 2026-10-28): /lsp is a table of the languages
// poly-lsp-mcp knows, like /mcp's servers and /rag's indexes, each one's
// child language server on, off or unset at the global, project and session
// scopes (switches.go). The list is poly-lsp-mcp's `lsp list --json`
// (v0.4.0); a language off here reaches the code server as --lsp-off, and
// one that is off by default (servers.yaml off_by_default) and on here as
// --lsp-on, so a change restarts it. Custom servers are poly-lsp-mcp's own
// (`lsp add`, servers.yaml in its daemon home): dun runs that command and
// never writes the file.

const lspKind = "lsp"

// lspLanguage is a row of `poly-lsp-mcp lsp list --json`.
type lspLanguage struct {
	Name       string   `json:"name"`
	Extensions []string `json:"extensions"`
	Source     string   `json:"source"` // builtin | user | project
	LSP        struct {
		Name  string   `json:"name"`
		Found bool     `json:"found"`
		Path  string   `json:"path"`
		Args  []string `json:"args"`
	} `json:"lsp"`
	Install *struct {
		Cmd string `json:"cmd"`
	} `json:"install"`
	OffByDefault bool   `json:"offByDefault"`
	FormatOnEdit bool   `json:"formatOnEdit"`
	Child        string `json:"child"` // running | stopped | down, when the root has state
}

// lspCLITimeout bounds one `lsp list|add|remove`.
const lspCLITimeout = 15 * time.Second

// runLSPCLI runs `<code server's command> lsp <args>` with its environment
// (its daemon home among it), in the workspace.
var runLSPCLI = func(ctx context.Context, command string, env []string, dir string, args ...string) ([]byte, error) {
	p, err := FindCommand(command)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, lspCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, append([]string{"lsp"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "LSP_REBUILD_DONE=1"), env...)
	out, err := cmd.Output()
	if err != nil {
		what := command + " lsp " + strings.Join(args, " ")
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%s: %s", what, strings.TrimSpace(firstLine(string(ee.Stderr))))
		}
		return out, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// codeServer is the code server's spec, under srvMu.
func (h *Harness) codeServer() (Server, bool) {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return h.spec(ServerCode)
}

// lspLanguages is poly-lsp-mcp's language registry for this workspace.
func (h *Harness) lspLanguages(ctx context.Context) ([]lspLanguage, error) {
	s, ok := h.codeServer()
	if !ok || s.Command == "" {
		return nil, fmt.Errorf("poly-lsp-mcp is not configured")
	}
	out, err := runLSPCLI(ctx, s.Command, s.Env, h.cfg.Workspace, "list", "--json", "--root", h.cfg.Workspace)
	if err != nil {
		return nil, err
	}
	var v struct {
		Languages []lspLanguage `json:"languages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("%s lsp list --json: %w (poly-lsp-mcp v0.4.0 has it)", s.Command, err)
	}
	return v.Languages, nil
}

func (h *Harness) lspSwitchKind() switchKind {
	return switchKind{name: lspKind, slash: "lsp", view: h.lspView, set: h.setLSPSwitch}
}

// lspView is the table: each language, its child server, on or off.
func (h *Harness) lspView() SwitchView {
	v := SwitchView{Kind: lspKind, Slash: "lsp", Modes: map[string]string{}}
	langs, err := h.lspLanguages(context.Background())
	if err != nil {
		v.Note = oneLine(err.Error(), 300)
	}
	set := h.switchSet(lspKind)
	for _, l := range langs {
		on, from, at := lspOn(set, l)
		v.Items = append(v.Items, SwitchState{SwitchItem: lspItem(l), On: on, From: from,
			Global: at[ScopeGlobal], Project: at[ScopeProject], Session: at[ScopeSession]})
	}
	v.Status = []string{
		"a language that is off keeps its tree-sitter index: definitions and references from it, edits checked by a parse only",
		"/lsp add <name> --ext .x --cmd <server> [--arg A] [--off-by-default] [--format-on-edit] · /lsp remove <name> (poly-lsp-mcp's servers.yaml)",
		"/lsp exclude|include <dir> · budget · index-anyway · restart: the code index",
	}
	return v
}

// lspOn resolves a language: a scope's switch, else its default (off for
// one off by default).
func lspOn(set switchSet, l lspLanguage) (bool, string, map[string]string) {
	on, from, at := set.on(l.Name)
	if from == "" {
		on = !l.OffByDefault
	}
	return on, from, at
}

// lspItem is a language as a row.
func lspItem(l lspLanguage) SwitchItem {
	server := l.LSP.Name
	switch {
	case server == "":
		server = "no language server"
	case l.LSP.Found:
		server += " · found"
	default:
		server += " · not installed"
		if l.Install != nil && l.Install.Cmd != "" {
			server += " (" + l.Install.Cmd + ")"
		}
	}
	src := l.Source
	if l.OffByDefault {
		src += " · off by default"
	}
	if l.FormatOnEdit {
		src += " · format on edit"
	}
	if l.Child != "" {
		src += " · " + l.Child
	}
	exts := make([]string, len(l.Extensions))
	for i, e := range l.Extensions {
		exts[i] = "." + strings.TrimPrefix(e, ".")
	}
	detail := ""
	if l.LSP.Path != "" {
		detail = strings.TrimSpace(l.LSP.Path + " " + strings.Join(l.LSP.Args, " "))
	}
	return SwitchItem{Name: l.Name, Description: server + " · " + strings.Join(exts, " "), Source: src, Detail: detail}
}

// lspArgs are the code server's --lsp-off / --lsp-on for this session.
func lspArgs(set switchSet, langs []lspLanguage) []string {
	var off, on []string
	for _, l := range langs {
		v, from, _ := lspOn(set, l)
		switch {
		case from == "":
		case !v && !l.OffByDefault:
			off = append(off, l.Name)
		case v && l.OffByDefault:
			on = append(on, l.Name)
		}
	}
	var args []string
	if len(off) > 0 {
		args = append(args, "--lsp-off", strings.Join(off, ","))
	}
	if len(on) > 0 {
		args = append(args, "--lsp-on", strings.Join(on, ","))
	}
	return args
}

// codeLSPArgs is lspArgs for the code server about to start; nothing when
// no scope switches a language (the list is not run then).
func (h *Harness) codeLSPArgs() []string {
	set := h.switchSet(lspKind)
	if len(set[ScopeGlobal].On)+len(set[ScopeProject].On)+len(set[ScopeSession].On) == 0 {
		return nil
	}
	langs, err := h.lspLanguages(context.Background())
	if err != nil {
		log.Printf("dun: %s: the language switches are not applied: %v", ServerCode, err)
		return nil
	}
	return lspArgs(set, langs)
}

// setLSPSwitch switches a language (or a glob of them) at scope; nil unsets
// it there. A running code server restarts with the result.
func (h *Harness) setLSPSwitch(pattern, scope string, on *bool) (string, error) {
	if _, err := path.Match(pattern, ""); err != nil {
		return "", fmt.Errorf("%q is not a glob: %v", pattern, err)
	}
	langs, err := h.lspLanguages(context.Background())
	if err != nil {
		return "", err
	}
	matched := 0
	for _, l := range langs {
		if m, _ := path.Match(pattern, l.Name); m {
			matched++
		}
	}
	if on != nil && matched == 0 {
		names := make([]string, len(langs))
		for i, l := range langs {
			names[i] = l.Name
		}
		return "", fmt.Errorf("no language %q (poly-lsp-mcp knows %s)", pattern, strings.Join(names, ", "))
	}
	if err := h.editSwitchFile(lspKind, scope, func(f *SwitchFile) {
		if on == nil {
			delete(f.On, pattern)
			return
		}
		if f.On == nil {
			f.On = map[string]bool{}
		}
		f.On[pattern] = *on
	}); err != nil {
		return "", err
	}
	what := "unset"
	if on != nil {
		what = onOffWord(*on)
	}
	n := fmt.Sprintf("%d language", matched)
	if matched != 1 {
		n += "s"
	}
	return fmt.Sprintf("lsp %s: %s for %s (%s)%s", pattern, what, scopeWords(scope), n, h.restartCode()), nil
}

// LSPEdit runs /lsp add <name> [flags] or /lsp remove <name>: poly-lsp-mcp's
// own `lsp add|remove`, which writes its servers.yaml. A running code server
// restarts: the daemon re-reads the file on the next open.
func (h *Harness) LSPEdit(ctx context.Context, verb string, args []string) (string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if verb == "add" {
			return "", fmt.Errorf("usage: /lsp add <name> --ext .x,.y --cmd <server> [--arg A]… [--env K=V]… [--off-by-default] [--format-on-edit]")
		}
		return "", fmt.Errorf("usage: /lsp remove <name>")
	}
	s, ok := h.codeServer()
	if !ok || s.Command == "" {
		return "", fmt.Errorf("poly-lsp-mcp is not configured")
	}
	out, err := runLSPCLI(ctx, s.Command, s.Env, h.cfg.Workspace, append([]string{verb}, args...)...)
	if err != nil {
		return "", err
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = "lsp " + verb + " " + args[0]
	}
	return msg + h.restartCode(), nil
}

// restartCode restarts a running code server, so a language change takes
// effect, and says so; "" when it is not running.
func (h *Harness) restartCode() string {
	if !h.mgr.ServerStarted(ServerCode) {
		return ""
	}
	if err := h.reloadServers(context.Background(), false, map[string]bool{ServerCode: true}); err != nil {
		return " · it did not restart: " + oneLine(err.Error(), 200)
	}
	return " · " + ServerCode + " restarts"
}

// lspFlagsKnown reports whether the binary's mcp help lists --lsp-off.
func lspFlagsKnown(help string) bool { return strings.Contains(help, "--lsp-off") }
