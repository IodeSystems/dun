package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

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
	if put.Help != "h" || len(put.steps()) != 2 || put.steps()[0].If != "dir" || put.steps()[1].Tool != "ingest" {
		t.Errorf("put: %+v", put)
	}
	if s := e.Slash.Commands["show"].steps(); len(s) != 1 || s[0].Tool != "m" {
		t.Errorf("an inline step is the one step: %+v", s)
	}
}

// The text after a command: shell quoting, the steps' flags taken out, each
// word a path against the workspace; placeholders filled in.
func TestParseCmdInput_Placeholders(t *testing.T) {
	c := ExtSlashCmd{Steps: []ExtStep{{Tool: "t"}, {Tool: "t", Unless: "--keep"}}}
	in, err := parseCmdInput(c, `'**/My *.LOG' --keep`, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if in.arg != "**/My *.LOG" || !in.flags["--keep"] {
		t.Errorf("arg %q flags %v", in.arg, in.flags)
	}
	home, _ := os.UserHomeDir()
	in, err = parseCmdInput(c, `docs/A.md "My Notes" /abs ~/x --other`, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/ws/docs/A.md", "/ws/My Notes", "/abs", filepath.Join(home, "x"), "/ws/--other"}; !slices.Equal(in.paths, want) {
		t.Errorf("paths %q", in.paths)
	}
	if in.arg != "docs/A.md My Notes /abs ~/x --other" || len(in.flags) != 0 {
		t.Errorf("an undeclared flag is text: %q %v", in.arg, in.flags)
	}
	if _, err := parseCmdInput(c, `'open`, "/ws"); err == nil {
		t.Error("an unbalanced quote is refused")
	}

	in = cmdInput{arg: "Some Text", index: "idx", project: "proj", workspace: "/ws"}
	args := map[string]any{"a": "{{arg}}", "i": "{{index}}:{{project}}", "w": "{{workspace}}/x",
		"p": "{{paths}}", "one": "{{path}}", "n": 3.0, "nest": []any{map[string]any{"q": "{{arg}}"}}, "b": true}
	got := in.expandArgs(args, "/one", []string{"/a", "/b"})
	want := map[string]any{"a": "Some Text", "i": "idx:proj", "w": "/ws/x", "p": []any{"/a", "/b"}, "one": "/one",
		"n": 3.0, "nest": []any{map[string]any{"q": "Some Text"}}, "b": true}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Errorf("args: %s", gb)
	}
	if args["a"] != "{{arg}}" {
		t.Error("the manifest's args are not changed")
	}
	argv := in.expandCLI([]string{"watch", "{{paths}}", "--index", "{{index}}", "{{path}}"}, "/one", []string{"/a", "/b"})
	if !slices.Equal(argv, []string{"watch", "/a", "/b", "--index", "idx", "/one"}) {
		t.Errorf("argv: %q", argv)
	}
}

// installProxy installs a user extension "proxy" whose server is the fake
// raglit (it serves MCP, and logs a CLI run), with the given commands.
func installProxy(t *testing.T, commands string) (*Harness, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	cmd, logPath := fakeBin(t, "raglit", "raglit")
	b, _ := json.Marshal(cmd)
	writeExt(t, filepath.Join(home, "extensions"), "proxy", `{"id":"proxy",
		"server":{"command":`+string(b)+`,"args":["serve","--project","pp","--index","ii","--daemon","http://d"],"disable":["ingest"]},
		"roles":{"index":{"cli":"raglit"}},
		"slash":{"name":"proxy","commands":`+commands+`}}`)
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []Server{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if errs := h.ExtensionErrors(); len(errs) != 0 {
		t.Fatalf("load: %v", errs)
	}
	return h, logPath, ws
}

// The proxy end to end through the fake: a path's kind picks its step (by
// stat, at run time), a cli step gets the serve args' store flags, a tool in
// server.disable is still called, the steps run in order, a flag skips one,
// an error stops the rest, and the text keeps its case.
func TestRunExtCommand_StepsDispatchAndOrder(t *testing.T) {
	h, logPath, ws := installProxy(t, `{
		"put":{"help":"FILE|DIR...","steps":[
			{"if":"dir","cli":["watch","{{path}}","--index","{{index}}"]},
			{"if":"file","tool":"ingest","args":{"index":"{{index}}","paths":"{{paths}}"}}]},
		"drop":{"steps":[
			{"tool":"manage_index","args":{"action":"ignore","index":"{{index}}","pattern":"{{arg}}"}},
			{"tool":"manage_index","args":{"action":"purge","index":"{{index}}","pattern":"{{arg}}","confirm":true},"unless":"--keep"}]},
		"fail":{"steps":[{"tool":"manage_index","args":{"action":"boom"}},{"tool":"ingest","args":{"paths":["x"]}}]},
		"show":{"tool":"manage_index","args":{"action":"status","project":"{{project}}"}}}`)
	ctx := context.Background()
	for _, d := range []string{"Docs A", "Docs B"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"One.md", "two.md"} {
		writeRule(t, filepath.Join(ws, f), "x")
	}
	if _, err := h.RunExtCommand(ctx, "proxy", "show", ""); err == nil || !strings.Contains(err.Error(), "/proxy on") {
		t.Errorf("a tool step needs the server: %v", err)
	}
	// A cli-only run needs no server.
	out, err := h.RunExtCommand(ctx, "proxy", "put", `'Docs A'`)
	if err != nil {
		t.Fatalf("put dir: %q %v", out, err)
	}
	logHas(t, logPath, "raglit watch "+filepath.Join(ws, "Docs A")+" --index ii --project pp --daemon http://d")

	if err := h.StartServer(ctx, "proxy"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(modelToolNames(h), "proxy__ingest") {
		t.Fatal("ingest is in disable: hidden from the model")
	}
	out, err = h.RunExtCommand(ctx, "proxy", "put", `One.md "Docs B" two.md`)
	if err != nil {
		t.Fatalf("put: %q %v", out, err)
	}
	logHas(t, logPath, "raglit watch "+filepath.Join(ws, "Docs B")+" --index ii")
	logHas(t, logPath, `call ingest {"index":"ii","paths":["`+filepath.Join(ws, "One.md")+`","`+filepath.Join(ws, "two.md")+`"]}`)
	if !strings.Contains(out, "› raglit watch "+filepath.Join(ws, "Docs B")) || !strings.Contains(out, "› ingest ") {
		t.Errorf("two steps ran, each named: %q", out)
	}
	if _, err := h.RunExtCommand(ctx, "proxy", "put", "missing.md"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("a missing path: %v", err)
	}
	if _, err := h.RunExtCommand(ctx, "proxy", "put", ""); err == nil || !strings.Contains(err.Error(), "usage: /proxy put FILE|DIR...") {
		t.Errorf("no text: %v", err)
	}

	calls := func() []string {
		var out []string
		for _, l := range fakeLog(logPath) {
			if strings.HasPrefix(l, "call manage_index") {
				out = append(out, strings.TrimPrefix(l, "call manage_index "))
			}
		}
		return out
	}
	out, err = h.RunExtCommand(ctx, "proxy", "drop", `'**/Big *.LOG'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"action":"ignore","index":"ii","pattern":"**/Big *.LOG"}`,
		`{"action":"purge","confirm":true,"index":"ii","pattern":"**/Big *.LOG"}`}
	if got := calls(); !slices.Equal(got, want) {
		t.Errorf("ignore, then purge, the pattern as typed:\n%q", got)
	}
	if !strings.Contains(out, "› manage_index") || !strings.Contains(out, `"confirm": true`) {
		t.Errorf("both replies, JSON indented: %s", out)
	}
	if _, err := h.RunExtCommand(ctx, "proxy", "drop", `*.tmp --keep`); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 3 || got[2] != `{"action":"ignore","index":"ii","pattern":"*.tmp"}` {
		t.Errorf("--keep skips the purge: %q", got)
	}

	before := len(fakeLog(logPath))
	out, err = h.RunExtCommand(ctx, "proxy", "fail", "")
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(out, "error:") {
		t.Errorf("a failed step: %q %v", out, err)
	}
	if l := fakeLog(logPath)[before:]; len(l) != 1 {
		t.Errorf("a failed step stops the rest: %q", l)
	}
	if out, err := h.RunExtCommand(ctx, "proxy", "show", ""); err != nil || !strings.Contains(out, `"project": "pp"`) {
		t.Errorf("show: %q %v", out, err)
	}
	if _, err := h.RunExtCommand(ctx, "proxy", "show", "extra"); err == nil {
		t.Error("a command with no placeholder for text takes none")
	}
}

// raglit's include, add-document, exclude, unexclude and excludes: what
// each asks of raglit, against this workspace's index (the serve args').
func TestRaglitCommands(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, logPath, _ := startFakeRaglit(t, ws, true)
	ctx := context.Background()
	idx, proj := raglitIndex(ws), raglitProject(ws)
	if err := os.MkdirAll(filepath.Join(ws, "Hand Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRule(t, filepath.Join(ws, "Notes.md"), "x")
	run := func(cmd, text string) string {
		t.Helper()
		out, err := h.RunExtCommand(ctx, ServerDocs, cmd, text)
		if err != nil {
			t.Fatalf("/rag %s %s: %q %v", cmd, text, out, err)
		}
		return out
	}

	// include: a directory is watched, a file is read once.
	run("include", `"Hand Book" Notes.md`)
	logHas(t, logPath, "raglit watch "+filepath.Join(ws, "Hand Book")+" --index "+idx+" --project "+proj)
	logHas(t, logPath, `call ingest {"index":"`+idx+`","paths":["`+filepath.Join(ws, "Notes.md")+`"]}`)
	// add-document: a directory too is read once, never watched.
	before := len(fakeLog(logPath))
	run("add-document", `"Hand Book"`)
	logHas(t, logPath, `call ingest {"index":"`+idx+`","paths":["`+filepath.Join(ws, "Hand Book")+`"]}`)
	for _, l := range fakeLog(logPath)[before:] {
		if strings.HasPrefix(l, "raglit watch") {
			t.Errorf("add-document watched: %s", l)
		}
	}

	// exclude: ignore, then purge with confirm; --keep only ignores.
	manage := func() []string {
		var out []string
		for _, l := range fakeLog(logPath) {
			if s, ok := strings.CutPrefix(l, "call manage_index "); ok {
				out = append(out, s)
			}
		}
		return out
	}
	run("exclude", `'**/*.LOG'`)
	run("exclude", `Build/** --keep`)
	run("unexclude", `'**/*.LOG'`)
	out := run("excludes", "")
	// prune: a report without --yes, the removal only with it.
	run("prune", "")
	run("prune", "--yes")
	want := []string{
		`{"action":"ignore","index":"` + idx + `","pattern":"**/*.LOG"}`,
		`{"action":"purge","confirm":true,"index":"` + idx + `","pattern":"**/*.LOG"}`,
		`{"action":"ignore","index":"` + idx + `","pattern":"Build/**"}`,
		`{"action":"unignore","index":"` + idx + `","pattern":"**/*.LOG"}`,
		`{"action":"status","index":"` + idx + `"}`,
		`{"action":"prune","index":"` + idx + `"}`,
		`{"action":"prune","confirm":true,"index":"` + idx + `"}`,
	}
	if got := manage(); !slices.Equal(got, want) {
		t.Errorf("manage_index calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out, `"action": "status"`) {
		t.Errorf("excludes shows the reply: %s", out)
	}
	if _, err := h.RunExtCommand(ctx, ServerDocs, "exclude", "--keep"); err == nil {
		t.Error("exclude needs a GLOB")
	}
}
