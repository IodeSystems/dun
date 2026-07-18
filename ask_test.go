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
	ask := func(_ context.Context, q string, opts []string, multi bool) (string, error) {
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
	ask := func(_ context.Context, _ string, _ []string, multi bool) (string, error) {
		gotMulti = multi
		return "x", nil
	}
	d := withAsk(nil, ask, nil)
	var tc llm.ToolCall
	tc.Function.Name = "ask_user"
	tc.Function.Arguments = `{"question":"Q"}`
	if _, err := d(context.Background(), tc); err != nil {
		t.Fatal(err)
	}
	if gotMulti {
		t.Fatal("multi should default to false when omitted")
	}
}
