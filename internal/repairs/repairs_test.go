package repairs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A month of ledger lines: old ones (a round with no repair field, the
// malformed_call_repair kind) and every event kind.
func fixture(t *testing.T, now time.Time) string {
	t.Helper()
	dir := t.TempDir()
	today := now.Format(time.RFC3339Nano)
	earlier := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.Local).Format(time.RFC3339Nano)
	if now.Day() == 1 {
		earlier = today
	}
	lastMonth := now.AddDate(0, -2, 0).Format(time.RFC3339Nano)
	lines := []string{
		// old lines, as written before the event kinds
		`{"t":"` + earlier + `","session":"s1","role":"main","provider":"openrouter","model":"gemini-4","prompt":9000,"cached":8000,"completion":40,"latency_ms":2000}`,
		`{"kind":"malformed_call_repair","t":"` + earlier + `","provider":"openrouter","model":"gemini-4","native_finish":"MALFORMED_FUNCTION_CALL"}`,
		`{"kind":"provider_fail_retry","t":"` + earlier + `","provider":"openrouter","model":"gemini-4","attempt":1,"max":3,"wait_ms":2000}`,
		`{"kind":"shrink","t":"` + earlier + `","before":9000,"after":4000}`,
		`{"kind":"outcome","t":"` + earlier + `","turn":3}`,
		// new
		`{"kind":"repair","t":"` + today + `","provider":"openrouter","model":"gemini-4","cause":"empty-reply"}`,
		`{"kind":"repair","t":"` + today + `","provider":"local","model":"qwen","cause":"empty-reply"}`,
		`{"kind":"repair_outcome","t":"` + today + `","provider":"openrouter","model":"gemini-4","cause":"empty-reply","outcome":"repaired","attempt":1}`,
		`{"kind":"repair_outcome","t":"` + today + `","provider":"local","model":"qwen","cause":"empty-reply","outcome":"failed","attempt":1}`,
		`{"kind":"repair_outcome","t":"` + today + `","provider":"openrouter","model":"gemini-4","cause":"provider_fail","outcome":"repaired","attempt":1}`,
		`{"t":"` + today + `","role":"main","provider":"openrouter","model":"gemini-4","prompt":10000,"cached":0,"completion":100,"latency_ms":3000,"cost":0.02,"cur":"USD","costed":true,"repair":"provider_fail"}`,
		`{"kind":"retry","t":"` + today + `","provider":"openrouter","model":"gemini-4","scope":"request","cause":"429","status":429,"wait_ms":61000}`,
		`{"kind":"retry","t":"` + today + `","provider":"openrouter","model":"gemini-4","scope":"request","outcome":"recovered"}`,
		`{"kind":"fold","t":"` + today + `","provider":"local","model":"qwen","cause":"overflow","before":90000,"after":12000}`,
		`{"kind":"cap_clamp","t":"` + today + `","provider":"https://api.example.test/v1","model":"m","key":"effort","sent":"high","wanted":"xhigh"}`,
		`{"kind":"server_restart","t":"` + today + `","server":"shell","outcome":"restarted"}`,
		`{"kind":"server_restart","t":"` + today + `","server":"shell","outcome":"gave_up"}`,
		`{"kind":"cancel","t":"` + today + `","cause":"user"}`,
		`{"kind":"tool_error","t":"` + today + `","tool":"read","cause":"malformed_json"}`,
		`{"kind":"tool_error","t":"` + today + `","tool":"read","cause":"error"}`,
		`{"kind":"tool_error","t":"` + today + `","tool":"node_edit","cause":"warned"}`,
		`{"kind":"cmd_time","t":"` + today + `","cmd":"/mcp","ms":900,"surface":"tui"}`,
		`{"kind":"cmd_time","t":"` + today + `","cmd":"/mcp","ms":500,"surface":"tui"}`,
		`{"kind":"cmd_time","t":"` + today + `","cmd":"/help","ms":20,"surface":"tui"}`,
		`not json`,
		// another month: not this one's
		`{"kind":"repair","t":"` + lastMonth + `","model":"gemini-4","cause":"repetition"}`,
	}
	u := now.UTC()
	if err := os.WriteFile(filepath.Join(dir, "rounds-"+u.Format("2006-01")+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func group(r Report, kind string) Group {
	for _, g := range r.Groups {
		if g.Kind == kind {
			return g
		}
	}
	return Group{}
}

func TestRead_GroupsAndCounts(t *testing.T) {
	now := time.Now()
	r := Read(fixture(t, now), now)
	er := group(r, "repair empty-reply")
	if er.Count.N != 2 || er.Count.OK != 1 || er.Count.Failed != 1 || er.ByModel["local · qwen"].Failed != 1 || er.ByModel["openrouter · gemini-4"].OK != 1 {
		t.Errorf("empty-reply %+v", er)
	}
	if g := group(r, "repair malformed-call"); g.Count.N != 1 {
		t.Errorf("old malformed_call_repair line %+v", g)
	}
	if g := group(r, "provider resend"); g.Count.N != 1 || g.Count.OK != 1 || g.Count.WaitMS != 2000 {
		t.Errorf("provider resend %+v", g)
	}
	if g := group(r, "retry request 429"); g.Count.N != 1 || g.Count.WaitMS != 61000 {
		t.Errorf("retry %+v", g)
	}
	if g := group(r, "retry request (ends)"); g.Count.OK != 1 {
		t.Errorf("retry ends %+v", g)
	}
	if g := group(r, "server restart shell"); g.Count.N != 2 || g.Count.OK != 1 || g.Count.Failed != 1 {
		t.Errorf("restarts %+v", g)
	}
	if g := group(r, "clamp effort"); g.ByModel["api.example.test · m"].N != 1 {
		t.Errorf("clamp %+v", g)
	}
	if g := group(r, "cancelled by user"); g.Count.N != 1 || g.Count.Today != 1 {
		t.Errorf("cancel %+v", g)
	}
	if g := group(r, "repair repetition"); g.Count.N != 0 {
		t.Errorf("another month's line counted: %+v", g)
	}
	if r.Cost.Rounds != 1 || r.Cost.Tokens != 10100 || r.Cost.Cost != 0.02 || r.Cost.WaitMS != 63000 || r.CostToday.Rounds != 1 {
		t.Errorf("cost %+v today %+v", r.Cost, r.CostToday)
	}
	if len(r.Tools) != 2 || r.Tools[0].Tool != "read" || r.Tools[0].N != 2 || r.Tools[1].ByCause["warned"] != 1 {
		t.Errorf("tools %+v", r.Tools)
	}
	if len(r.Cmds) != 2 || r.Cmds[0].Cmd != "/mcp" || r.Cmds[0].Max != 900 || r.Cmds[0].Last != 500 || r.Cmds[0].N != 2 {
		t.Errorf("cmds %+v", r.Cmds)
	}
	var gem Latency
	for _, l := range r.Latency {
		if l.Model == "gemini-4" {
			gem = l
		}
	}
	if gem.Rounds != 2 || gem.LatencyP95 != 3000 {
		t.Errorf("latency %+v", r.Latency)
	}
}

func TestRender_Repairs(t *testing.T) {
	now := time.Now()
	out := RenderRepairs(Read(fixture(t, now), now), Plain)
	for _, want := range []string{"cost of repairs", "this month", "10.1k", "$0.02", "1m3s",
		"repair empty-reply", "local · qwen", "server restart shell", "tool errors", "malformed_json 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
}

func TestRender_EmptyMonth(t *testing.T) {
	out := RenderRepairs(Read(t.TempDir(), time.Now()), Plain)
	if strings.Count(out, "none recorded this month") != 2 {
		t.Errorf("empty month:\n%s", out)
	}
	if !strings.Contains(RenderCmds(nil, Plain), "none timed yet") {
		t.Error("empty cmds")
	}
}

func TestRender_CmdsFlagSlow(t *testing.T) {
	out := RenderCmds([]Cmd{{Cmd: "/mcp", Surface: "tui", N: 2, P50: 500, P95: 900, Max: 900, Last: 500}, {Cmd: "/help", Surface: "tui", N: 1, P50: 20, P95: 20, Max: 20, Last: 20}}, Plain)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "900ms!") || strings.Contains(lines[2], "!") {
		t.Errorf("flags:\n%s", out)
	}
}

func TestTable_Widths(t *testing.T) {
	out := Table("", []string{"a", "n"}, [][]string{{"long name", "1"}, {"b", "12345"}})
	want := "a              n\nlong name      1\nb          12345"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}
