package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// withAsk must parse `multi` and pass it (and options) through to the AskFunc.
func TestWithAsk_MultiPassthrough(t *testing.T) {
	var gotQ string
	var gotOpts []string
	var gotMulti bool
	ask := func(_ context.Context, q string, opts []string, multi bool, _ []string) (string, error) {
		gotQ, gotOpts, gotMulti = q, opts, multi
		return "A, C", nil
	}
	// inner is nil: the ask_user path must not fall through to it.
	d := withAsk(nil, ask, nil)

	var tc llm.ToolCall
	tc.Function.Name = "ask_user"
	tc.Function.Arguments = `{"question":"Pick","options":["A","B","C"],"multi":true}`
	res, err := d(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if res != "A, C" {
		t.Fatalf("result = %q, want the AskFunc's answer", res)
	}
	if gotQ != "Pick" || len(gotOpts) != 3 || !gotMulti {
		t.Fatalf("args not passed through: q=%q opts=%v multi=%v", gotQ, gotOpts, gotMulti)
	}
}

// A missing `multi` defaults to false (single-select).
func TestWithAsk_MultiDefaultsFalse(t *testing.T) {
	var gotMulti = true
	ask := func(_ context.Context, _ string, _ []string, multi bool, _ []string) (string, error) {
		gotMulti = multi
		return "x", nil
	}
	d := withAsk(nil, ask, nil)
	var tc llm.ToolCall
	tc.Function.Name = "ask_user"
	tc.Function.Arguments = `{"question":"Q","options":["a","b"],"recommended":"a"}`
	if _, err := d(context.Background(), tc); err != nil {
		t.Fatal(err)
	}
	if gotMulti {
		t.Fatal("multi should default to false when omitted")
	}
}

// A question with fewer than two options, or no recommendation among them,
// goes back to the model; the user never sees it. USER 2026-10-21.
func TestWithAsk_RequiresOptionsAndARecommendation(t *testing.T) {
	asked := ""
	ask := func(_ context.Context, q string, _ []string, _ bool, rec []string) (string, error) {
		asked = q + "|" + strings.Join(rec, ",")
		return "ok", nil
	}
	d := withAsk(nil, ask, nil)
	for args, wantErr := range map[string]bool{
		`{"question":"Did you mean it?"}`:                          true,
		`{"question":"Q","options":["only"],"recommended":"only"}`: true,
		`{"question":"Q","options":["a"," "],"recommended":"a"}`:   true,
		`{"question":"Q","options":["a","b"]}`:                     true,
		`{"question":"Q","options":["a","b"],"recommended":"c"}`:   true,
		`{"question":"Q","options":["a","b"],"recommended":"b"}`:   false,
		`{"question":"Q","options":["a","b","c"],"multi":true}`:    false,
		`{"question":"","options":["a","b"],"recommended":"a"}`:    true,
		// recommended as an array: several only with multi, each an option.
		`{"question":"Q","options":["a","b"],"recommended":["a"]}`:                      false,
		`{"question":"Q","options":["a","b"],"recommended":["a","b"]}`:                  true,
		`{"question":"Q","options":["a","b"],"recommended":[]}`:                         true,
		`{"question":"Q","options":["a","b","c"],"multi":true,"recommended":["a","c"]}`: false,
		`{"question":"Q","options":["a","b","c"],"multi":true,"recommended":[]}`:        false,
		`{"question":"Q","options":["a","b","c"],"multi":true,"recommended":["d"]}`:     true,
	} {
		asked = ""
		var tc llm.ToolCall
		tc.Function.Name = "ask_user"
		tc.Function.Arguments = args
		res, err := d(context.Background(), tc)
		if err != nil {
			t.Fatal(err)
		}
		gotErr := len(res) > 6 && res[:6] == "ERROR:"
		if gotErr != wantErr {
			t.Errorf("%s: result %q", args, res)
		}
		if wantErr && asked != "" {
			t.Errorf("%s: the user was asked anyway", args)
		}
	}
	if asked != "" && asked != "Q|" { // the last valid case (multi) passes no recommendation
		t.Logf("last ask: %s", asked)
	}
}

// For multi, `recommended` may name several options: as an array, or as a
// comma-separated string whose parts are all options. A string that is itself
// an option stays one, commas and all.
func TestAskRecommended(t *testing.T) {
	opts := []string{"lint", "tests", "a, b"}
	for raw, want := range map[string]string{
		`["lint","tests"]`: "lint|tests",
		`"lint, tests"`:    "lint|tests",
		`"a, b"`:           "a, b",
		`"lint"`:           "lint",
		`""`:               "",
		`null`:             "",
		`"lint, nope"`:     "lint, nope",
	} {
		got := strings.Join(askRecommended([]byte(raw), opts, true), "|")
		if got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	// Single-select never splits.
	if got := askRecommended([]byte(`"lint, tests"`), opts, false); len(got) != 1 {
		t.Errorf("single split the string: %q", got)
	}
}

// The description has to make the case for multi, or models never set it
// (seen: every question one pick, one recommendation).
func TestAskToolDef_TeachesMulti(t *testing.T) {
	td := askToolDef()
	for _, want := range []string{"Pick several (`multi`: true)", "which of these to fix", "Pick one"} {
		if !strings.Contains(td.Function.Description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}
