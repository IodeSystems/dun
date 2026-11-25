package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/project"
	"github.com/iodesystems/dun/internal/sesslist"
	"github.com/iodesystems/dun/proto"
)

var sessionsNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// sessionsDundDown is a dund that cannot be reached.
func sessionsDundDown() ([]proto.SessionSummary, error) { return nil, errors.New("no socket") }

// sessionsWorkspace registers a workspace and writes one transcript per ask
// into its session directory: id -> the user's asks, in order. mod sets the
// file's modification time (the last activity of a saved session).
func sessionsWorkspace(t *testing.T, ids map[string][]string, mod map[string]time.Time) string {
	t.Helper()
	ws := t.TempDir()
	if _, err := project.Ensure(ws); err != nil {
		t.Fatal(err)
	}
	dir := harness.RootDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for id, asks := range ids {
		var b strings.Builder
		b.WriteString(`{"Kind":"notification","Content":"note"}` + "\n")
		for _, a := range asks {
			e, _ := json.Marshal(map[string]string{"Kind": "user", "Content": a})
			b.Write(e)
			b.WriteString("\n" + `{"Kind":"assistant","Content":"ok"}` + "\n")
		}
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, ok := mod[id]; ok {
			os.Chtimes(p, m, m)
		}
	}
	return ws
}

func runSessions(t *testing.T, o sessionsOpts) (out, errs string, rc int) {
	t.Helper()
	var ob, eb bytes.Buffer
	if o.now.IsZero() {
		o.now = sessionsNow
	}
	if o.width == 0 {
		o.width = 80
	}
	rc = sessionsRun(o, &ob, &eb)
	return ob.String(), eb.String(), rc
}

// Rows are newest first by last activity, one line each: id, status, age,
// the LAST request (not the first).
func TestSessions_RenderAndOrder(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := sessionsWorkspace(t,
		map[string][]string{
			"20261101-000000": {"old question"},
			"20261125-115700": {"first ask", "the   latest\nask"},
		},
		map[string]time.Time{
			"20261101-000000": sessionsNow.Add(-72 * time.Hour),
			"20261125-115700": sessionsNow.Add(-3 * time.Minute),
		})
	out, errs, rc := runSessions(t, sessionsOpts{ws: ws, n: 20, dund: sessionsDundDown})
	if rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines:\n%s", out)
	}
	if !strings.HasPrefix(lines[0], "20261125-115700") || !strings.HasPrefix(lines[1], "20261101-000000") {
		t.Errorf("not newest first:\n%s", out)
	}
	for _, want := range []string{"saved", "3m ago", "the latest ask"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q lacks %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "first ask") {
		t.Errorf("shows the first ask, not the last: %q", lines[0])
	}
	if !strings.Contains(lines[1], "3d ago") {
		t.Errorf("age of the old one: %q", lines[1])
	}
	if !strings.Contains(errs, "dund is not running") {
		t.Errorf("no dund-down note: %q", errs)
	}
}

// With dund up, its list says which sessions run; a dund session with no
// file here still shows, and another workspace's is left out without --all.
func TestSessions_DundStatus(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := sessionsWorkspace(t,
		map[string][]string{"20261125-100000": {"a"}, "20261125-110000": {"b"}},
		map[string]time.Time{"20261125-100000": sessionsNow.Add(-2 * time.Hour), "20261125-110000": sessionsNow.Add(-time.Hour)})
	other := sessionsWorkspace(t, map[string][]string{"20261125-090000": {"elsewhere"}}, nil)
	live := func() ([]proto.SessionSummary, error) {
		return []proto.SessionSummary{
			{ID: "20261125-100000", Workspace: ws, Running: true, LastActive: sessionsNow.Add(-time.Minute).Format(time.RFC3339)},
			{ID: "20261125-110000", Workspace: ws},
			{ID: "20261125-090000", Workspace: other, Running: true},
		}, nil
	}
	out, errs, _ := runSessions(t, sessionsOpts{ws: ws, n: 20, dund: live})
	if strings.Contains(out, "20261125-090000") {
		t.Errorf("other workspace listed without --all:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "20261125-100000") || !strings.Contains(lines[0], "running") {
		t.Errorf("running session first with its status:\n%s", out)
	}
	if !strings.Contains(lines[1], "stopped") {
		t.Errorf("known, no engine: want stopped:\n%s", out)
	}
	if errs != "" {
		t.Errorf("stderr with dund up: %q", errs)
	}
}

// --all lists every registered workspace and adds the directory, with ~.
func TestSessions_All(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := sessionsWorkspace(t, map[string][]string{"20261125-100000": {"in a"}}, nil)
	b := sessionsWorkspace(t, map[string][]string{"20261125-110000": {"in b"}}, nil)
	out, _, _ := runSessions(t, sessionsOpts{ws: a, all: true, n: 20, dund: sessionsDundDown, width: 400})
	for _, want := range []string{"in a", "in b"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "in a") > strings.Index(out, "in b") {
		t.Errorf("this workspace's session must come first:\n%s", out)
	}
	if got := sesslist.TildeDir(filepath.Join(home, "x", "y")); got != "~/x/y" {
		t.Errorf("tildeDir = %q", got)
	}
	_ = b
}

// -n cuts the rows and says how many are left; the request is cut to the
// terminal width.
func TestSessions_LimitAndTruncate(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ids := map[string][]string{}
	for _, id := range []string{"20261125-000001", "20261125-000002", "20261125-000003"} {
		ids[id] = []string{strings.Repeat("word ", 60)}
	}
	ws := sessionsWorkspace(t, ids, map[string]time.Time{
		"20261125-000001": sessionsNow.Add(-3 * time.Hour),
		"20261125-000002": sessionsNow.Add(-2 * time.Hour),
		"20261125-000003": sessionsNow.Add(-time.Hour),
	})
	out, _, _ := runSessions(t, sessionsOpts{ws: ws, n: 2, dund: sessionsDundDown, width: 60})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 || lines[2] != "…1 more (-n 0 for all)" {
		t.Fatalf("want 2 rows and a more line:\n%s", out)
	}
	for _, l := range lines[:2] {
		if n := len([]rune(l)); n > 60 {
			t.Errorf("line is %d wide, want <= 60: %q", n, l)
		}
		if !strings.HasSuffix(l, "…") {
			t.Errorf("a cut request ends in …: %q", l)
		}
	}
}

// --json is one object with the rows; an empty workspace is an empty list.
func TestSessions_JSON(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := sessionsWorkspace(t, map[string][]string{"20261125-100000": {"hi"}}, nil)
	out, _, rc := runSessions(t, sessionsOpts{ws: ws, n: 20, json: true, dund: sessionsDundDown})
	var got struct {
		Sessions []sesslist.Row `json:"sessions"`
		Total    int            `json:"total"`
		Dund     bool           `json:"dund"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || rc != 0 {
		t.Fatalf("rc %d, %v:\n%s", rc, err, out)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Last != "hi" || got.Sessions[0].Status != "saved" || got.Dund {
		t.Errorf("got %+v", got)
	}
	empty := t.TempDir()
	out, _, _ = runSessions(t, sessionsOpts{ws: empty, n: 20, json: true, dund: sessionsDundDown})
	if !strings.Contains(out, `"sessions": []`) {
		t.Errorf("empty list should be []:\n%s", out)
	}
}

// A session file with no user message is not a session worth listing.
func TestSessions_SkipsNeverAsked(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := sessionsWorkspace(t, map[string][]string{"20261125-100000": {"real"}, "20261125-110000": {}}, nil)
	out, _, _ := runSessions(t, sessionsOpts{ws: ws, n: 20, dund: sessionsDundDown})
	if strings.Contains(out, "20261125-110000") || !strings.Contains(out, "real") {
		t.Errorf("never-asked session listed:\n%s", out)
	}
}

func TestSessions_Args(t *testing.T) {
	var out, errw bytes.Buffer
	if rc := sessionsCmd([]string{"--help"}, &out, &errw); rc != 0 || !strings.Contains(out.String(), "dun sessions") {
		t.Errorf("--help: rc %d", rc)
	}
	for _, bad := range [][]string{{"--bogus"}, {"-n"}, {"-n", "x"}, {"-n", "-1"}} {
		if rc := sessionsCmd(bad, &out, &errw); rc != 2 {
			t.Errorf("%v: rc %d, want 2", bad, rc)
		}
	}
}

func TestSessions_IsSubcommand(t *testing.T) {
	if !isSubcommand("sessions") {
		t.Error("sessions is not a subcommand")
	}
}
