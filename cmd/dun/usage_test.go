package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

func writeLedger(t *testing.T, lines ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	dir := filepath.Join(home, "stats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rounds-2026-10.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ledgerLine(ago time.Duration, role, model string, prompt, cached, comp int, cost float64) string {
	ts := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	c := ""
	if cost > 0 {
		c = fmt.Sprintf(`,"cost":%v,"cur":"USD","costed":true`, cost)
	}
	return fmt.Sprintf(`{"t":%q,"role":%q,"provider":"p","model":%q,"prompt":%d,"cached":%d,"completion":%d%s}`, ts, role, model, prompt, cached, comp, c)
}

func TestUsageReport(t *testing.T) {
	writeLedger(t,
		ledgerLine(time.Hour, "main", "m1", 1000, 600, 100, 0.5),
		ledgerLine(2*time.Hour, "model", "m2", 200, 0, 50, 0),
		ledgerLine(3*24*time.Hour, "main", "m1", 1000, 0, 100, 0.25),
		ledgerLine(40*24*time.Hour, "main", "m1", 500, 0, 10, 0),
		"{torn")
	var out, errb bytes.Buffer
	if rc := usageCmd([]string{"--json", "--no-net"}, &out, &errb); rc != 0 {
		t.Fatal(rc, errb.String())
	}
	var rep usageReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	all := rep.Periods[2]
	if all.Requests != 4 || all.Prompt != 2700 || all.Cached != 600 || all.Completion != 260 || all.Costed != 2 {
		t.Fatalf("all time: %+v", all)
	}
	if got := all.Cost["USD"]; got != 0.75 {
		t.Fatalf("cost %v", got)
	}
	if w := rep.Periods[1]; w.Requests != 3 || w.Prompt != 2200 {
		t.Fatalf("week: %+v", w)
	}
	if d := rep.Periods[0]; d.Requests != 2 && d.Requests != 1 { // 2h ago may cross midnight
		t.Fatalf("today: %+v", d)
	}
	if rep.ByRole["main"].Requests != 3 || rep.ByRole["model"].Requests != 1 {
		t.Fatalf("roles: %+v", rep.ByRole)
	}
	if rep.ByModel["p/m1"].Requests != 3 || rep.ByModel["p/m2"] == nil {
		t.Fatalf("models: %+v", rep.ByModel)
	}
	if rep.Budget.Status != "skipped" {
		t.Fatalf("budget: %+v", rep.Budget)
	}
}

func TestUsageText(t *testing.T) {
	writeLedger(t, ledgerLine(time.Minute, "main", "m1", 1000, 500, 100, 0.5))
	t.Setenv("DUN_URL", "http://example.invalid")
	old := budgetFetcher
	defer func() { budgetFetcher = old }()
	budgetFetcher = func(context.Context, string, string, string) (llm.KeyBudget, error) {
		return llm.KeyBudget{Reported: true, HasLimit: true, Limit: 10, HasRemaining: true, Remaining: 4, Spend: 6, Currency: "USD", Interval: "monthly"}, nil
	}
	var out, errb bytes.Buffer
	if rc := usageCmd(nil, &out, &errb); rc != 0 {
		t.Fatal(rc, errb.String())
	}
	s := out.String()
	for _, want := range []string{"all time", "p/m1", "50%", "0.5000 USD", "4.0000 USD remaining of 10.0000 (40%)", "main: session model"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	budgetFetcher = func(context.Context, string, string, string) (llm.KeyBudget, error) {
		return llm.KeyBudget{}, llm.ErrBudgetUnsupported
	}
	out.Reset()
	usageCmd(nil, &out, &errb)
	if !strings.Contains(out.String(), "left: not known: the endpoint does not report") {
		t.Errorf("unsupported:\n%s", out.String())
	}
}

func TestUsageEmptyLedger(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if rc := usageCmd([]string{"--no-net"}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "no rounds recorded") {
		t.Fatal(rc, out.String())
	}
}

// A server scripted in mcpshell: what its schemas would have added, per
// request and in total, and money only as an upper bound (USER 2026-10-27).
func TestUsage_ScriptedSavings(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	lines := []string{
		`{"t":"2026-10-26T10:00:00Z","model":"m","prompt":100000,"completion":10,"cost":0.01,"cur":"USD","costed":true,"scripted":{"vay":82000}}`,
		`{"t":"2026-10-27T10:00:00Z","model":"m","prompt":100000,"completion":10,"cost":0.01,"cur":"USD","costed":true,"scripted":{"vay":82900}}`,
		`{"t":"2026-10-27T11:00:00Z","model":"m","prompt":1000,"completion":10}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "rounds-2026-10.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := readUsage(dir, now)
	s := rep.Scripted["vay"]
	if s == nil || s.Requests != 2 || s.Tokens != 164900 || s.Latest != 82900 {
		t.Fatalf("scripted: %+v", s)
	}
	out := renderUsage(rep)
	for _, want := range []string{"scripted in mcpshell", "~82,900 tokens a request now", "2 requests", "~164,900 tokens not sent", "≤ 0.0165 USD at the full prompt rate", "cached tokens cost less"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}
