// Package repairs reads the repairs, retries and timings out of the stats
// ledger ($DUN_HOME/stats/rounds-YYYY-MM.jsonl; harness/roundstats.go and
// harness/repairs.go write it) for /usage repairs, /usage perf and
// `dun usage --repairs`. One reader, one renderer, for the TUI and the CLI.
//
// The ledger mixes round stats (no kind) and events (a kind); the readers
// of rounds skip the events, and this reads both. Lines older than the event
// kinds (a round with no repair field, the malformed_call_repair kind) parse
// as they are: an old line is a round or an event it already was.
package repairs

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// line is every field the reader uses, round or event.
type line struct {
	Kind     string    `json:"kind"`
	Time     time.Time `json:"t"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	// rounds
	Prompt     int     `json:"prompt"`
	Cached     int     `json:"cached"`
	Completion int     `json:"completion"`
	LatencyMS  int64   `json:"latency_ms"`
	PromptN    int     `json:"prompt_n"`
	PromptMS   float64 `json:"prompt_ms"`
	GenN       int     `json:"gen_n"`
	GenMS      float64 `json:"gen_ms"`
	Cost       float64 `json:"cost"`
	Currency   string  `json:"cur"`
	Costed     bool    `json:"costed"`
	Repair     string  `json:"repair"`
	// events
	Cause   string `json:"cause"`
	Outcome string `json:"outcome"`
	Scope   string `json:"scope"`
	Attempt int    `json:"attempt"`
	WaitMS  int64  `json:"wait_ms"`
	Tool    string `json:"tool"`
	Server  string `json:"server"`
	Key     string `json:"key"`
	Cmd     string `json:"cmd"`
	MS      int64  `json:"ms"`
	Surface string `json:"surface"`
}

// Count is a count this month and today, with outcomes where they apply
// (OK: repaired, recovered, restarted; Failed: failed, gave up).
type Count struct {
	N      int   `json:"n"`
	Today  int   `json:"today"`
	OK     int   `json:"ok,omitempty"`
	Failed int   `json:"failed,omitempty"`
	WaitMS int64 `json:"wait_ms,omitempty"`
}

func (c *Count) add(o Count) {
	c.N += o.N
	c.Today += o.Today
	c.OK += o.OK
	c.Failed += o.Failed
	c.WaitMS += o.WaitMS
}

// Group is one kind of repair, and its count by provider and model.
type Group struct {
	Kind    string           `json:"kind"`
	Count   Count            `json:"count"`
	ByModel map[string]Count `json:"by_model"`
}

// Cost is what the repair rounds cost: the rounds that were resends or
// repairs (RoundStat.Repair), and the time spent waiting on backoff.
type Cost struct {
	Rounds   int     `json:"rounds"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"cur,omitempty"`
	WaitMS   int64   `json:"wait_ms"`
}

// Tool is one tool's error results, by class.
type Tool struct {
	Tool    string         `json:"tool"`
	N       int            `json:"n"`
	ByCause map[string]int `json:"by_cause"`
}

// Latency is one model's round timing: wall clock, and llama.cpp's prefill
// and generation speeds when the endpoint reported them.
type Latency struct {
	Model       string  `json:"model"`
	Rounds      int     `json:"rounds"`
	LatencyP50  int64   `json:"latency_p50_ms"`
	LatencyP95  int64   `json:"latency_p95_ms"`
	Timed       int     `json:"timed,omitempty"`
	PrefillP50  float64 `json:"prefill_p50_tps,omitempty"`
	PrefillP95  float64 `json:"prefill_p95_tps,omitempty"`
	GenerateP50 float64 `json:"gen_p50_tps,omitempty"`
	GenerateP95 float64 `json:"gen_p95_tps,omitempty"`
}

// Cmd is one command's times where they were measured.
type Cmd struct {
	Cmd     string `json:"cmd"`
	Surface string `json:"surface"`
	N       int    `json:"n"`
	P50     int64  `json:"p50_ms"`
	P95     int64  `json:"p95_ms"`
	Max     int64  `json:"max_ms"`
	Last    int64  `json:"last_ms"`
}

// Report is the month's repairs, costs, tool errors, latencies and
// command times.
type Report struct {
	Since     time.Time `json:"since"` // the month's start, local
	Day       time.Time `json:"day"`   // today's start, local
	Lines     int       `json:"lines"` // ledger lines read
	Groups    []Group   `json:"groups"`
	Cost      Cost      `json:"cost"`
	CostToday Cost      `json:"cost_today"`
	Tools     []Tool    `json:"tools,omitempty"`
	Latency   []Latency `json:"latency,omitempty"`
	Cmds      []Cmd     `json:"cmds,omitempty"`
}

// Read reads dir's ledger for the month now is in.
func Read(dir string, now time.Time) Report {
	now = now.Local()
	r := Report{
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local),
		Day:   time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local),
	}
	groups := map[string]*Group{}
	tools := map[string]*Tool{}
	type lat struct {
		ms              []int64
		prefill, genTPS []float64
	}
	lats := map[string]*lat{}
	type cmdAcc struct {
		ms   []int64
		last int64
	}
	cmds := map[string]*cmdAcc{}
	grp := func(kind, model string, c Count) {
		g := groups[kind]
		if g == nil {
			g = &Group{Kind: kind, ByModel: map[string]Count{}}
			groups[kind] = g
		}
		g.Count.add(c)
		m := g.ByModel[model]
		m.add(c)
		g.ByModel[model] = m
	}
	for _, path := range files(dir, now) {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var l line
			if json.Unmarshal(sc.Bytes(), &l) != nil || l.Time.Before(r.Since) || l.Time.After(now.Add(time.Minute)) {
				continue
			}
			r.Lines++
			today := !l.Time.Before(r.Day)
			one := Count{N: 1}
			if today {
				one.Today = 1
			}
			model := modelKey(l.Provider, l.Model)
			switch l.Kind {
			case "":
				if l.Repair != "" {
					addCost(&r.Cost, l)
					if today {
						addCost(&r.CostToday, l)
					}
				}
				a := lats[l.Model]
				if a == nil {
					a = &lat{}
					lats[l.Model] = a
				}
				a.ms = append(a.ms, l.LatencyMS)
				if l.PromptN >= 512 && l.PromptMS > 0 {
					a.prefill = append(a.prefill, float64(l.PromptN)/(l.PromptMS/1000))
				}
				if l.GenN >= 16 && l.GenMS > 0 {
					a.genTPS = append(a.genTPS, float64(l.GenN)/(l.GenMS/1000))
				}
			case "repair":
				grp("repair "+l.Cause, model, one)
			case "malformed_call_repair": // before the repair kind
				grp("repair malformed-call", model, one)
			case "repair_outcome":
				o := outcome(l.Outcome, l.Attempt)
				switch l.Cause {
				case "provider_fail":
					grp("provider resend", model, o)
				case "length_cut":
					grp("length cut", model, o)
				default:
					grp("repair "+l.Cause, model, o)
				}
			case "provider_fail_retry":
				one.WaitMS = l.WaitMS
				grp("provider resend", model, one)
				waited(&r, l, today)
			case "empty_reply":
				grp("empty reply (turn ended)", model, one)
			case "retry":
				if l.Outcome != "" {
					o := outcome(map[string]string{"recovered": "repaired", "gave_up": "failed"}[l.Outcome], 1)
					grp("retry "+l.Scope+" (ends)", model, o)
					continue
				}
				one.WaitMS = l.WaitMS
				grp("retry "+l.Scope+" "+l.Cause, model, one)
				waited(&r, l, today)
			case "length_cut":
				grp("length cut", model, one)
			case "fold":
				grp("fold "+l.Cause, model, one)
			case "fold_failed":
				grp("fold failed", model, one)
			case "cap_clamp":
				grp("clamp "+l.Key, model, one)
			case "server_restart":
				one.OK = b2i(l.Outcome == "restarted")
				one.Failed = 1 - one.OK
				grp("server restart "+l.Server, model, one)
			case "cancel":
				grp("cancelled by "+l.Cause, model, one)
			case "turn_cap":
				grp("turn cap", model, one)
			case "tool_error":
				t := tools[l.Tool]
				if t == nil {
					t = &Tool{Tool: l.Tool, ByCause: map[string]int{}}
					tools[l.Tool] = t
				}
				t.N++
				t.ByCause[l.Cause]++
			case "cmd_time":
				k := l.Cmd + "\x00" + l.Surface
				a := cmds[k]
				if a == nil {
					a = &cmdAcc{}
					cmds[k] = a
				}
				a.ms = append(a.ms, l.MS)
				a.last = l.MS
			}
		}
		f.Close()
	}
	for _, g := range groups {
		r.Groups = append(r.Groups, *g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		if a, b := r.Groups[i].Count.N, r.Groups[j].Count.N; a != b {
			return a > b
		}
		return r.Groups[i].Kind < r.Groups[j].Kind
	})
	for _, t := range tools {
		r.Tools = append(r.Tools, *t)
	}
	sort.Slice(r.Tools, func(i, j int) bool {
		if r.Tools[i].N != r.Tools[j].N {
			return r.Tools[i].N > r.Tools[j].N
		}
		return r.Tools[i].Tool < r.Tools[j].Tool
	})
	for m, a := range lats {
		if m == "" {
			m = "(unknown)"
		}
		l := Latency{Model: m, Rounds: len(a.ms), LatencyP50: pctI(a.ms, 50), LatencyP95: pctI(a.ms, 95), Timed: len(a.prefill)}
		l.PrefillP50, l.PrefillP95 = pctF(a.prefill, 50), pctF(a.prefill, 95)
		l.GenerateP50, l.GenerateP95 = pctF(a.genTPS, 50), pctF(a.genTPS, 95)
		r.Latency = append(r.Latency, l)
	}
	sort.Slice(r.Latency, func(i, j int) bool {
		if r.Latency[i].Rounds != r.Latency[j].Rounds {
			return r.Latency[i].Rounds > r.Latency[j].Rounds
		}
		return r.Latency[i].Model < r.Latency[j].Model
	})
	for k, a := range cmds {
		cmd, surface, _ := strings.Cut(k, "\x00")
		c := Cmd{Cmd: cmd, Surface: surface, N: len(a.ms), P50: pctI(a.ms, 50), P95: pctI(a.ms, 95), Last: a.last}
		for _, v := range a.ms {
			c.Max = max(c.Max, v)
		}
		r.Cmds = append(r.Cmds, c)
	}
	SortCmds(r.Cmds)
	return r
}

// SortCmds puts the slowest first (p95, then max), by name after that.
func SortCmds(cs []Cmd) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].P95 != cs[j].P95 {
			return cs[i].P95 > cs[j].P95
		}
		if cs[i].Max != cs[j].Max {
			return cs[i].Max > cs[j].Max
		}
		return cs[i].Cmd+cs[i].Surface < cs[j].Cmd+cs[j].Surface
	})
}

// files is the month's ledger files: this UTC month's and the one before,
// which holds the local month's first hours west of UTC.
func files(dir string, now time.Time) []string {
	u := now.UTC()
	prev := time.Date(u.Year(), u.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	return []string{
		filepath.Join(dir, "rounds-"+prev.Format("2006-01")+".jsonl"),
		filepath.Join(dir, "rounds-"+u.Format("2006-01")+".jsonl"),
	}
}

// modelKey is "provider · model"; an endpoint URL as provider is its host.
func modelKey(provider, model string) string {
	if u, err := url.Parse(provider); err == nil && u.Host != "" {
		provider = u.Host
	}
	if model == "" {
		model = "(unknown)"
	}
	if provider == "" {
		return model
	}
	return provider + " · " + model
}

func outcome(o string, n int) Count {
	n = max(n, 1)
	switch o {
	case "repaired":
		return Count{OK: n}
	case "failed":
		return Count{Failed: n}
	}
	return Count{}
}

func addCost(c *Cost, l line) {
	c.Rounds++
	c.Tokens += l.Prompt + l.Completion
	if l.Costed && (c.Currency == "" || c.Currency == l.Currency) {
		c.Currency = l.Currency
		c.Cost += l.Cost
	}
}

func waited(r *Report, l line, today bool) {
	r.Cost.WaitMS += l.WaitMS
	if today {
		r.CostToday.WaitMS += l.WaitMS
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func pctI(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)*p/100, len(s)-1)]
}

func pctF(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[min(len(s)*p/100, len(s)-1)]
}
