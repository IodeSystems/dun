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
		{120, "ctx 41.3k/131k · cache  96% · ttft  0.8s · pp  1.2k/s · tg   45/s"},
		{100, "ctx 41.3k/131k · cache  96% · ttft  0.8s · tg   45/s"},
		{80, "ctx 41.3k/131k · tg   45/s"},
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
// width, so nothing moves; the start of the next request clears the old
// figures to dashes in place.
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
	if !strings.HasSuffix(b, "ctx 41.7k/131k · cache    – · ttft     – · pp       – · tg      –") {
		t.Errorf("at start %q", b)
	}
	if !strings.HasSuffix(c, "ttft 12.3s · pp       – · tg  150/s") {
		t.Errorf("live %q", c)
	}
	// After the request ends the figures stay.
	m = m.handleEvent(reqEv(t, reqDone))
	m = m.handleEvent(evMsg{"type": "done"})
	if d := stripANSI(m.headView()); !strings.HasSuffix(d, "tg   45/s") {
		t.Errorf("after the turn %q", d)
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
	if !strings.HasSuffix(head, "ctx 41.3k · ttft  0.8s · pp  1.2k/s · tg   45/s") {
		t.Errorf("bar %q", head)
	}
	// Reported once, 0% is a figure.
	r.CacheReported = true
	m = m.handleEvent(reqEv(t, r))
	if head := stripANSI(m.headView()); !strings.Contains(head, "cache   0%") {
		t.Errorf("a reported 0%% cache: %q", head)
	}
}
