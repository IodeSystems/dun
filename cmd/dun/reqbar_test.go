package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun/proto"
)

// reqEv is a request event as it arrives: through JSON.
func reqEv(t *testing.T, r proto.Request) evMsg {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	ev := evMsg{}
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	ev["type"] = "request"
	return ev
}

func barModel(t *testing.T, w int, rs ...proto.Request) tuiModel {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.url, m.model = "http://llm/v1", "chat"
	m.w, m.h = w, 30
	m.branch = "worktree-agent-a257eaba" // a title as long as a real one
	for _, r := range rs {
		m = m.handleEvent(reqEv(t, r))
	}
	return m
}

var reqDone = proto.Request{Phase: "done", TTFTMS: 800, Prompt: 5000, Cached: 4800, CacheReported: true,
	PPS: 1234, Gen: 310, TGS: 45.2, Exact: true, Ctx: 41300, Window: 131072}

// The bar at 120, 100 and 80 columns: right-aligned, the title untouched,
// pieces dropped pp → ttft → cache as the width shrinks, ctx and tg kept
// longest; at 48 none.
func TestReqBar_Widths(t *testing.T) {
	cases := []struct {
		w    int
		want string // the stats, as plain text
	}{
		{120, "ctx 41.3k/131k · cache  96% · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45"},
		{100, "ctx 41.3k/131k · cache  96% · tg   45/s   ~45"},
		{80, "ctx 41.3k/131k · tg   45/s   ~45"},
		{48, ""},
	}
	const title = "dun · ws ⎇ worktree-agent-a257eaba · chat"
	for _, c := range cases {
		m := barModel(t, c.w, reqDone)
		head := stripANSI(m.headView())
		if !strings.HasPrefix(head, title) {
			t.Errorf("%d: the title moved: %q", c.w, head)
		}
		if c.want == "" {
			if head != title {
				t.Errorf("%d: stats on a narrow bar: %q", c.w, head)
			}
			continue
		}
		if !strings.HasSuffix(head, c.want) {
			t.Errorf("%d: bar %q\nwant it to end %q", c.w, head, c.want)
		}
		if got := ansi.StringWidth(head); got != c.w {
			t.Errorf("%d: bar is %d cells", c.w, got)
		}
	}
}

// No bounce: a live update with other numbers draws the stats at the same
// width, so nothing moves. The start of the next request keeps the last
// figures (it used to clear them to dashes, so between requests the bar said
// nothing — USER 2026-10-28); a live update replaces only what it measured.
func TestReqBar_NoBounce(t *testing.T) {
	m := barModel(t, 120, reqDone)
	a := stripANSI(m.headView())
	m = m.handleEvent(reqEv(t, proto.Request{Phase: "start", Ctx: 41700, Window: 131072}))
	b := stripANSI(m.headView())
	m = m.handleEvent(reqEv(t, proto.Request{Phase: "live", TTFTMS: 12300, Gen: 9, TGS: 150, Ctx: 41709, Window: 131072}))
	c := stripANSI(m.headView())
	if len([]rune(a)) != len([]rune(b)) || len([]rune(b)) != len([]rune(c)) {
		t.Errorf("widths moved:\n%q\n%q\n%q", a, b, c)
	}
	if !strings.HasSuffix(b, "ctx 41.7k/131k · cache  96% · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45") {
		t.Errorf("at start %q", b)
	}
	if !strings.HasSuffix(c, "ttft 12.3s · pp  1.2k/s ~1.2k · tg  150/s   ~45") {
		t.Errorf("live %q", c)
	}
	// After the request ends the figures stay.
	m = m.handleEvent(reqEv(t, reqDone))
	m = m.handleEvent(evMsg{"type": "done"})
	if d := stripANSI(m.headView()); !strings.HasSuffix(d, "tg   45/s   ~45") {
		t.Errorf("after the turn %q", d)
	}
}

// The average is weighted by tokens: a long reply at 30/s and a short one at
// 90/s average near 30, not 60.
func TestReqBar_AverageIsTokenWeighted(t *testing.T) {
	long, short := reqDone, reqDone
	long.TGS, long.Gen = 30, 900
	short.TGS, short.Gen = 90, 100
	m := barModel(t, 120, long, short)
	if head := stripANSI(m.headView()); !strings.HasSuffix(head, "tg   90/s   ~36") {
		t.Errorf("bar %q, want the last (90/s) and the weighted average (~36)", head)
	}
}

// A provider that never reported cached tokens shows no cache piece at all;
// a window nobody stated shows the count alone.
func TestReqBar_CacheNeverReported(t *testing.T) {
	r := reqDone
	r.Cached, r.CacheReported, r.Window = 0, false, 0
	m := barModel(t, 120, r)
	head := stripANSI(m.headView())
	if strings.Contains(head, "cache") {
		t.Errorf("cache shown though never reported: %q", head)
	}
	if !strings.HasSuffix(head, "ctx 41.3k · ttft  0.8s · pp  1.2k/s ~1.2k · tg   45/s   ~45") {
		t.Errorf("bar %q", head)
	}
	// Reported once, 0% is a figure.
	r.CacheReported = true
	m = m.handleEvent(reqEv(t, r))
	if head := stripANSI(m.headView()); !strings.Contains(head, "cache   0%") {
		t.Errorf("a reported 0%% cache: %q", head)
	}
}
