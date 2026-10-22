package dun

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// withAsk must parse `multi` and pass it (and options) through to the AskFunc.
func TestWithAsk_MultiPassthrough(t *testing.T) {
	var gotQ string
	var gotOpts []string
	var gotMulti bool
	ask := func(_ context.Context, q string, opts []string, multi bool, _ string) (string, error) {
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
	ask := func(_ context.Context, _ string, _ []string, multi bool, _ string) (string, error) {
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
	ask := func(_ context.Context, q string, _ []string, _ bool, rec string) (string, error) {
		asked = q + "|" + rec
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
