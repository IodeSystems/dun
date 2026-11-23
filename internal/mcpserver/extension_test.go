package mcpserver

import (
	"strings"
	"testing"
)

// The bundled manifests parse, and are what DefaultServers runs.
func TestBundledExtensions_AreRaglitAndPolyLsp(t *testing.T) {
	var ids []string
	for _, e := range BundledExtensions() {
		ids = append(ids, e.ID+"/"+e.SlashName()+"/"+e.Source)
	}
	if strings.Join(ids, " ") != "raglit/rag/bundled poly-lsp/lsp/bundled" {
		t.Fatalf("bundled: %v", ids)
	}
	raglit, _ := bundledExt(ServerDocs)
	for _, h := range []string{HookTurnBefore, HookWorkspaceOpen, HookMemorySave, HookSkillsChanged, HookRulesChanged, HookMCPDocs} {
		if !raglit.Subscribes(h) {
			t.Errorf("raglit does not take %s", h)
		}
	}
	if _, ok := bundledExt("code"); !ok { // a pre-rename id still finds it
		t.Errorf("bundled: no poly-lsp by its old id")
	}
	ws := t.TempDir()
	for _, s := range DefaultServers(ws, "") {
		if strings.Contains(strings.Join(s.Args, " "), "{{") {
			t.Errorf("%s: placeholder left in %v", s.ID, s.Args)
		}
	}
}

// A slash command's error names the field: the command, the step, what is
// wrong with it.
func TestExtSlashCmd_ErrorsNameTheField(t *testing.T) {
	cli := `"roles":{"index":{"cli":"raglit"}},`
	for cmd, field := range map[string]string{
		`{"help":"h"}`:                                         "slash.commands.c: has a tool, a cli or steps",
		`{"tool":"t","steps":[{"tool":"u"}]}`:                  "slash.commands.c: has a tool, a cli or steps",
		`{"tool":"t","cli":["x"]}`:                             "slash.commands.c.tool:",
		`{"steps":[{"tool":"t"},{"args":{}}]}`:                 "slash.commands.c.steps[1].tool:",
		`{"cli":[]}`:                                           "slash.commands.c.cli: empty",
		`{"cli":["x"],"args":{"a":"b"}}`:                       "slash.commands.c.args:",
		`{"tool":"t","if":"link","args":{"p":"{{path}}"}}`:     "slash.commands.c.if:",
		`{"tool":"t","if":"file"}`:                             "slash.commands.c.if: the step uses no",
		`{"tool":"t","unless":"keep"}`:                         "slash.commands.c.unless:",
		`{"tool":"t","with":"yes"}`:                            "slash.commands.c.with:",
		`{"tool":"t","args":{"a":"{{path}}","b":"{{paths}}"}}`: "slash.commands.c: {{path}} and {{paths}}",
		`{"tool":"t","args":{"a":"{{nope}}"}}`:                 "slash.commands.c: unknown placeholder {{nope}}",
		`{"tool":"t","args":{"a":["x {{paths}}"]}}`:            "slash.commands.c: {{paths}} is a whole value",
	} {
		body := `{"id":"a","server":{"command":"raglit"},` + cli + `"slash":{"name":"a","commands":{"c":` + cmd + `}}}`
		if _, err := ParseExtension([]byte(body)); err == nil || !strings.HasPrefix(err.Error(), field) {
			t.Errorf("%s: want %q, got %v", cmd, field, err)
		}
	}
	// A cli step needs the role that names the binary.
	if _, err := ParseExtension([]byte(`{"id":"a","server":{"command":"x"},"slash":{"name":"a","commands":{"c":{"cli":["x"]}}}}`)); err == nil ||
		!strings.Contains(err.Error(), "needs roles.index.cli") {
		t.Errorf("cli without roles.index.cli: %v", err)
	}
	ok := `{"id":"a","server":{"command":"raglit"},` + cli + `"slash":{"name":"a","commands":{
		"put":{"help":"h","steps":[{"if":"dir","cli":["watch","{{path}}","--index","{{index}}"]},
			{"if":"file","tool":"ingest","args":{"index":"{{index}}","paths":"{{paths}}"}}]},
		"drop":{"steps":[{"tool":"m","args":{"pattern":"{{arg}}"}},{"tool":"m","args":{"confirm":true},"unless":"--keep"}]},
		"show":{"tool":"m","args":{"action":"status","project":"{{project}}","in":"{{workspace}}"}}}}}`
	e, err := ParseExtension([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	put := e.Slash.Commands["put"]
	if put.Help != "h" || len(put.AllSteps()) != 2 || put.AllSteps()[0].If != "dir" || put.AllSteps()[1].Tool != "ingest" {
		t.Errorf("put: %+v", put)
	}
	if s := e.Slash.Commands["show"].AllSteps(); len(s) != 1 || s[0].Tool != "m" {
		t.Errorf("an inline step is the one step: %+v", s)
	}
}

// A manifest error starts with the field it is about.
func TestParseExtension_ErrorsNameTheField(t *testing.T) {
	for body, field := range map[string]string{
		`{"id":"Bad","server":{"command":"x"}}`:                                                             "id:",
		`{"id":"a","server":{}}`:                                                                            "server:",
		`{"id":"a","server":{"command":"x","url":"http://y"}}`:                                              "server:",
		`{"id":"a","server":{"id":"b","command":"x"}}`:                                                      "server.id:",
		`{"id":"a","server":{"command":"x","via":"sideways"}}`:                                              "server.via:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"Hi"}}`:                                         "slash.name:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"a","commands":{"on":{"tool":"t"}}}}`:           "slash.commands.on:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"a","commands":{"go":{}}}}`:                     "slash.commands.go:",
		`{"id":"a","server":{"command":"x"},"roles":{"search":{}}}`:                                         "roles.search.tool:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{}}}`:                                          "roles.index:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{"tool":"i"}}}`:                                "roles.index.forget:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{"cli":"other"}}}`:                             "roles.index.cli:",
		`{"id":"a","server":{"command":"x"},"hooks":["turn.before"]}`:                                       "hooks[0]:",
		`{"id":"a","server":{"command":"x"},"roles":{"search":{"tool":"s"}},"hooks":["turn.before","x.y"]}`: "hooks[1]:",
		`{"id":"a","server":{"command":"x"},"hooks":["memory.save"]}`:                                       "hooks[0]:",
		`{"id":"a","server":{"command":"x"},"env":{"1X":"v"}}`:                                              "env.1X:",
		`{"id":"a","server":{"command":"x"},"skills":"../up"}`:                                              "skills:",
		`{"id":"a","server":{"command":"x"},"hoks":[]}`:                                                     `unknown field "hoks"`,
	} {
		_, err := ParseExtension([]byte(body))
		if err == nil || !strings.HasPrefix(err.Error(), field) && !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want %q, got %v", body, field, err)
		}
	}
}

// An older published manifest carries host_hooks (the removed AGENTS.md
// guard). It loads, and nothing in it arms a hook.
func TestParseExtension_DeprecatedHostHooksAreIgnored(t *testing.T) {
	e, err := ParseExtension([]byte(`{"id":"a","server":{"command":"x"},"host_hooks":{"POLY_LSP_HOOK":"agents_md_guard"}}`))
	if err != nil {
		t.Fatalf("a manifest with host_hooks must load: %v", err)
	}
	if e.Server.Command != "x" {
		t.Errorf("server: %+v", e.Server)
	}
}
