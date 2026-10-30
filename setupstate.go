package dun

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Install hints, from poly-lsp-mcp (commit f064670).
//
// poly-lsp falls back to tree-sitter for a language whose child LSP is not
// installed: no error, and so nothing said what that costs. After its
// bring-up, before its first tool reply, it sends notifications/message from
// logger "poly-lsp-mcp/setup", level "warning" when a child LSP is missing
// and "info" when none is:
//
//	{"languages": [{lang, files, lsp: {name, found, path}, adds,
//	  install: {cmd, needs: ["go"|"node"|"python"|"system"]}}], "missing": N}
//
// Only languages with files in the root are listed. lsp is absent for a
// language that is tree-sitter only by design; adds and install are there
// only when the LSP is missing. `poly-lsp-mcp doctor --json` carries the same
// list under "languages" (ParseDoctorLanguages).
//
// What dun does with it: keeps the report on the server (ServerState.Setup:
// /lsp shows the table, /mcp info counts the missing), and says one
// quiet line per session about the missing servers worth it (SetupLine).
// It never installs anything: the servers need their own toolchains, and
// the install is the person's. An older poly-lsp sends none, and nothing
// changes.

// SetupLSP is one child LSP's state on the machine that runs the server.
type SetupLSP struct {
	Name  string `json:"name"`
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
}

// SetupInstall is how to get a child LSP and what that needs on the machine.
type SetupInstall struct {
	Cmd   string   `json:"cmd"`
	Needs []string `json:"needs"` // "go", "node", "python", "system"
}

// SetupLang is one language the project holds.
type SetupLang struct {
	Lang    string        `json:"lang"`
	Files   int           `json:"files"`
	LSP     *SetupLSP     `json:"lsp,omitempty"` // nil: tree-sitter only by design
	Adds    string        `json:"adds,omitempty"`
	Install *SetupInstall `json:"install,omitempty"`
}

// SetupReport is a setup notification's data: poly-lsp's languages, or
// raglit's capabilities, roles and extractors (ragsetup.go).
type SetupReport struct {
	Languages []SetupLang `json:"languages"`
	Missing   int         `json:"missing"`

	Capabilities     []SetupCapability `json:"capabilities,omitempty"`
	Roles            []SetupRole       `json:"roles,omitempty"`
	Extractors       []SetupExtractor  `json:"extractors,omitempty"`
	ExtractorsSeenBy string            `json:"extractorsSeenBy,omitempty"`
}

// MissingLSPs are the languages whose child LSP is not installed.
func (r SetupReport) MissingLSPs() []SetupLang {
	var out []SetupLang
	for _, l := range r.Languages {
		if l.LSP != nil && !l.LSP.Found {
			out = append(out, l)
		}
	}
	return out
}

// setupLoggerSuffix marks a notifications/message logger as a setup report:
// "poly-lsp-mcp/setup".
const setupLoggerSuffix = "/setup"

// parseSetupNote reads a setup notification's params, ok=false when they
// are not one (another logger, or neither languages nor capabilities).
func parseSetupNote(params map[string]any) (SetupReport, bool) {
	logger, _ := params["logger"].(string)
	if !strings.HasSuffix(logger, setupLoggerSuffix) {
		return SetupReport{}, false
	}
	b, err := json.Marshal(params["data"])
	if err != nil {
		return SetupReport{}, false
	}
	var r SetupReport
	if json.Unmarshal(b, &r) != nil || (len(r.Languages) == 0 && len(r.Capabilities) == 0) {
		return SetupReport{}, false
	}
	return r, true
}

// ParseDoctorLanguages reads the languages out of `poly-lsp-mcp doctor
// --json`, ok=false when out is not that (an older build prints an error).
func ParseDoctorLanguages(out []byte) (SetupReport, bool) {
	var d struct {
		Languages []SetupLang `json:"languages"`
	}
	if json.Unmarshal(out, &d) != nil || d.Languages == nil {
		return SetupReport{}, false
	}
	r := SetupReport{Languages: d.Languages}
	r.Missing = len(r.MissingLSPs())
	return r, true
}

// setupMeaningfulShare and setupMeaningfulFiles say which missing language
// servers are worth a line in the conversation: a language is a meaningful
// part of the project when its files are at least a tenth of the listed
// languages' files (which are the indexed files), or at least 200 files. A
// share, not a count: dun's own tree has 12 Python helper scripts among
// ~1,300 files, and a missing pylsp must not nag there, while 12 files in a
// 60-file project are a fifth of it. /lsp lists every language
// regardless.
const (
	setupMeaningfulShare = 10 // one in this many (a tenth)
	setupMeaningfulFiles = 200
)

// setupAdds is what a child LSP adds, in a line's words; the report's own
// "adds" (poly-lsp's terms) is in the /lsp table.
const setupAdds = "references, rename and diagnostics"

// toolchainBins are the programs a "needs" word stands for, any one of
// which on PATH is the toolchain; "system" (the OS package manager) is not
// checked. toolchainNames is how a line names them.
var (
	toolchainBins  = map[string][]string{"go": {"go"}, "node": {"node"}, "python": {"python3", "python", "pipx"}}
	toolchainNames = map[string]string{"go": "Go", "node": "Node", "python": "Python"}
)

// onPath reports whether a program is on PATH. A variable for tests.
var onPath = func(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

// ToolchainMissing names the toolchains in needs that are not on PATH
// ("Node"), the ones a line should say.
func ToolchainMissing(needs []string) []string {
	var out []string
	for _, n := range needs {
		bins, checked := toolchainBins[n]
		if !checked {
			continue
		}
		found := false
		for _, b := range bins {
			found = found || onPath(b)
		}
		if !found {
			out = append(out, toolchainNames[n])
		}
	}
	return out
}

// LangName is a language as a person writes it ("go" → "Go").
func LangName(lang string) string {
	switch lang {
	case "typescript":
		return "TypeScript"
	case "javascript":
		return "JavaScript"
	case "cpp":
		return "C++"
	case "":
		return ""
	}
	return strings.ToUpper(lang[:1]) + lang[1:]
}

// SetupLine is the one quiet line for a report, "" when no missing language
// server is in a meaningful part of the project (setupMeaningfulShare). One
// language: its server, what it adds and the install command, with the
// toolchain it needs when that is not on PATH. Several: one line naming
// them, /<slash> for the commands. A capability report's (raglit) is
// CapabilityLine's.
func SetupLine(r SetupReport, slash string) string {
	if len(r.Capabilities) > 0 {
		return CapabilityLine(r, slash)
	}
	total := 0
	for _, l := range r.Languages {
		total += l.Files
	}
	var worth []SetupLang
	for _, l := range r.MissingLSPs() {
		if l.Files >= setupMeaningfulFiles || l.Files*setupMeaningfulShare >= total {
			worth = append(worth, l)
		}
	}
	switch len(worth) {
	case 0:
		return ""
	case 1:
		l := worth[0]
		s := fmt.Sprintf("poly-lsp: %s would add %s for %s (%s)", l.LSP.Name, setupAdds, LangName(l.Lang), filesOf(l.Files))
		if l.Install == nil {
			return s + " — /" + slash
		}
		s += " — " + l.Install.Cmd
		if miss := ToolchainMissing(l.Install.Needs); len(miss) > 0 {
			s += " (needs " + strings.Join(miss, ", ") + ")"
		}
		return s
	}
	var servers, langs []string
	for _, l := range worth {
		servers = append(servers, l.LSP.Name)
		langs = append(langs, fmt.Sprintf("%s (%s)", LangName(l.Lang), filesOf(l.Files)))
	}
	return fmt.Sprintf("poly-lsp: %s would add %s for %s — /%s has the install commands",
		joinAnd(servers), setupAdds, joinAnd(langs), slash)
}

// filesOf is "412 files".
func filesOf(n int) string {
	if n == 1 {
		return "1 file"
	}
	return Grouped(n) + " files"
}

// joinAnd is "a", "a and b", "a, b and c".
func joinAnd(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// noteSetup keeps a setup report for id and returns the line to say about
// it: once per session, and "" when nothing missing is worth one.
func (h *Harness) noteSetup(id string, r SetupReport) string {
	slash := h.slashFor(id)
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	if h.setup == nil {
		h.setup = map[string]*SetupReport{}
	}
	h.setup[id] = &r
	if h.setupSaid[id] {
		return ""
	}
	line := SetupLine(r, slash)
	// A capability line points at the person's providers: without a model
	// client there are none to pick from.
	if len(r.Capabilities) > 0 && !hasProvider(h.cfg.Client) {
		line = ""
	}
	if line == "" {
		return ""
	}
	if h.setupSaid == nil {
		h.setupSaid = map[string]bool{}
	}
	h.setupSaid[id] = true
	return line
}

// hasProvider reports whether c is a model the person logged in to: not
// nil, and not the stand-in a logged-out dun runs on (it says Providerless).
func hasProvider(c any) bool {
	if c == nil {
		return false
	}
	p, ok := c.(interface{ Providerless() bool })
	return !ok || !p.Providerless()
}

// SetupTable is every listed language as rows, for /lsp: its files,
// its language server found (and where) or missing, what a missing one
// adds, the install command and what that needs.
func SetupTable(r SetupReport) string {
	rows := []string{"language servers, by files:"}
	for _, l := range r.Languages {
		row := fmt.Sprintf("  %s · %s", LangName(l.Lang), filesOf(l.Files))
		switch {
		case l.LSP == nil:
			row += " · tree-sitter only"
		case l.LSP.Found:
			row += " · " + l.LSP.Name + " found"
			if l.LSP.Path != "" {
				row += " at " + l.LSP.Path
			}
		default:
			row += " · " + l.LSP.Name + " missing"
		}
		rows = append(rows, row)
		if l.LSP == nil || l.LSP.Found {
			continue
		}
		if l.Adds != "" {
			rows = append(rows, "    adds: "+l.Adds)
		}
		if l.Install != nil {
			ins := "    install: " + l.Install.Cmd
			if len(l.Install.Needs) > 0 {
				ins += " · needs " + strings.Join(l.Install.Needs, ", ")
				if miss := ToolchainMissing(l.Install.Needs); len(miss) > 0 {
					ins += " (not on PATH: " + strings.Join(miss, ", ") + ")"
				}
			}
			rows = append(rows, ins)
		}
	}
	return strings.Join(append(rows, "dun does not install them; each needs its own toolchain"), "\n")
}

// parseDaemonNote reads a "<server>/daemon" notification: the server's
// shared daemon upgrading, upgraded or failed (poly-lsp-mcp 9e227d7). Its
// data carries one line in "message".
func parseDaemonNote(params map[string]any) (string, bool) {
	logger, _ := params["logger"].(string)
	if !strings.HasSuffix(logger, "/daemon") {
		return "", false
	}
	data, _ := params["data"].(map[string]any)
	msg, _ := data["message"].(string)
	msg = strings.TrimSpace(msg)
	return msg, msg != ""
}
