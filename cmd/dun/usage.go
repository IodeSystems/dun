package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

// `dun --usage`: what dun has used, read from the stats ledger
// ($DUN_HOME/stats/rounds-YYYY-MM.jsonl, dun/roundstats.go) without starting a
// session, a TUI, a server or dund. One line there is one model response, from
// every client the engine builds.
//
// What the ledger cannot say, and this report does not guess:
//   - root vs sub-agent: a sub-agent on the session's own model shares its
//     client, so its rounds are recorded as role "main". Only role "model" (a
//     sub-agent or side call on another model) and "vision" are separable.
//   - the cost of a round whose endpoint did not report one; those rounds count
//     in the tokens and not in the cost, and the report says how many did.
//
// "What is left" is the one network read: the active provider's key budget
// (llm.Client.KeyBudget), with a short timeout; --no-net skips it.

type usageTotals struct {
	Requests   int                `json:"requests"`
	Prompt     int                `json:"prompt_tokens"`
	Cached     int                `json:"cached_tokens"`
	CacheWrite int                `json:"cache_write_tokens,omitempty"`
	Completion int                `json:"completion_tokens"`
	Costed     int                `json:"costed_requests"`
	Cost       map[string]float64 `json:"cost,omitempty"` // by currency
}

// CacheRate is cached / prompt, ok=false when no prompt was recorded.
func (t usageTotals) CacheRate() (float64, bool) {
	if t.Prompt <= 0 {
		return 0, false
	}
	return float64(t.Cached) / float64(t.Prompt), true
}

func (t *usageTotals) add(r dun.RoundStat) {
	t.Requests++
	t.Prompt += r.Prompt
	t.Cached += r.Cached
	t.CacheWrite += r.CacheWrite
	t.Completion += r.Completion
	if r.Costed {
		t.Costed++
		if t.Cost == nil {
			t.Cost = map[string]float64{}
		}
		t.Cost[r.Currency] += r.Cost
	}
}

type usageBudget struct {
	Status    string   `json:"status"` // reported, unsupported, error, skipped, none
	Detail    string   `json:"detail,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Spend     float64  `json:"spend,omitempty"`
	Limit     *float64 `json:"limit,omitempty"`
	Remaining *float64 `json:"remaining,omitempty"`
	Currency  string   `json:"currency,omitempty"`
	Interval  string   `json:"interval,omitempty"`
	ResetAt   string   `json:"reset_at,omitempty"`
}

type usagePeriod struct {
	Name string `json:"name"`
	usageTotals
}

type usageReport struct {
	Now     time.Time               `json:"now"`
	Since   time.Time               `json:"first_round,omitempty"`
	Periods []usagePeriod           `json:"periods"`
	ByModel map[string]*usageTotals `json:"by_model"` // all time, key "provider/model"
	ByRole  map[string]*usageTotals `json:"by_role"`  // all time: main, model, vision
	Budget  usageBudget             `json:"budget"`
	Notes   []string                `json:"notes,omitempty"`
}

// roleNote says what each ledger role holds, as far as the ledger knows.
var roleNote = map[string]string{
	"main":   "session model: root and its same-model sub-agents",
	"model":  "other models: sub-agents and side calls",
	"vision": "vision fallback",
}

// readUsage totals every ledger line in dir, relative to now.
func readUsage(dir string, now time.Time) usageReport {
	rep := usageReport{Now: now, ByModel: map[string]*usageTotals{}, ByRole: map[string]*usageTotals{}}
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	week := now.AddDate(0, 0, -7)
	pt, pw, pa := &usageTotals{}, &usageTotals{}, &usageTotals{}
	get := func(mp map[string]*usageTotals, k string) *usageTotals {
		if mp[k] == nil {
			mp[k] = &usageTotals{}
		}
		return mp[k]
	}
	files, _ := filepath.Glob(filepath.Join(dir, "rounds-*.jsonl"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			var r dun.RoundStat
			if line == "" || json.Unmarshal([]byte(line), &r) != nil || r.Time.IsZero() {
				continue
			}
			pa.add(r)
			if rep.Since.IsZero() || r.Time.Before(rep.Since) {
				rep.Since = r.Time
			}
			if !r.Time.Before(today) {
				pt.add(r)
			}
			if !r.Time.Before(week) {
				pw.add(r)
			}
			name := r.Model
			if name == "" {
				name = "(unknown)"
			}
			if r.Provider != "" {
				name = r.Provider + "/" + name
			}
			get(rep.ByModel, name).add(r)
			role := r.Role
			if role == "" {
				role = "(unrecorded)"
			}
			get(rep.ByRole, role).add(r)
		}
	}
	rep.Periods = []usagePeriod{{"today", *pt}, {"last 7 days", *pw}, {"all time", *pa}}
	if pa.Requests == 0 {
		rep.Notes = append(rep.Notes, "no rounds recorded in "+dir)
		return rep
	}
	if pa.Costed < pa.Requests {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d of %d requests reported a cost; the rest are in the tokens, not the cost", pa.Costed, pa.Requests))
	}
	if pa.Prompt > 0 && pa.Cached == 0 {
		rep.Notes = append(rep.Notes, "no cached tokens recorded: the endpoint reports none, or nothing was reused")
	}
	rep.Notes = append(rep.Notes, "root and same-model sub-agents are not told apart: both are role main")
	return rep
}

// budgetFetcher reads the active provider's key budget; a test replaces it.
var budgetFetcher = func(ctx context.Context, url, key, model string) (llm.KeyBudget, error) {
	return llm.NewClient(url, key, model).KeyBudget(ctx)
}

func readBudget(noNet bool) usageBudget {
	if noNet {
		return usageBudget{Status: "skipped", Detail: "--no-net"}
	}
	fc := loadConfig()
	p, ok := fc.active()
	url, key := firstNonEmpty(os.Getenv("DUN_URL"), p.URL), firstNonEmpty(os.Getenv("DUN_LLM_KEY"), p.Key)
	if !ok && url == "" {
		return usageBudget{Status: "none", Detail: "no provider logged in"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := budgetFetcher(ctx, url, key, firstNonEmpty(os.Getenv("DUN_MODEL"), fc.Model))
	out := usageBudget{Provider: p.Name}
	switch {
	case errors.Is(err, llm.ErrBudgetUnsupported):
		out.Status, out.Detail = "unsupported", "the endpoint does not report a key budget"
	case err != nil:
		out.Status, out.Detail = "error", err.Error()
	case !b.Reported:
		out.Status, out.Detail = "unsupported", "the endpoint reported no budget"
	default:
		out.Status, out.Spend, out.Currency, out.Interval = "reported", b.Spend, b.Currency, b.Interval
		if b.HasLimit {
			out.Limit = &b.Limit
		}
		if b.HasRemaining {
			out.Remaining = &b.Remaining
		}
		if !b.ResetAt.IsZero() {
			out.ResetAt = b.ResetAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func usageCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dun --usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	noNet := fs.Bool("no-net", false, "do not ask the provider what is left")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: dun --usage [--json] [--no-net]")
		return 2
	}
	rep := readUsage(dun.DefaultStatsDir(), time.Now())
	rep.Budget = readBudget(*noNet)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	io.WriteString(stdout, renderUsage(rep))
	return 0
}

func costText(t usageTotals) string {
	if t.Costed == 0 {
		return "-"
	}
	cs := make([]string, 0, len(t.Cost))
	for cur := range t.Cost {
		cs = append(cs, cur)
	}
	sort.Strings(cs)
	var parts []string
	for _, c := range cs {
		s := fmt.Sprintf("%.4f", t.Cost[c])
		if c != "" {
			s += " " + c
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " + ")
}

func rateText(t usageTotals) string {
	if r, ok := t.CacheRate(); ok {
		return fmt.Sprintf("%.0f%%", 100*r)
	}
	return "-"
}

func usageRow(b *strings.Builder, w int, name string, t usageTotals) {
	fmt.Fprintf(b, "  %-*s %8s %12s %11s %6s  %s\n", w, name, grouped(t.Requests), grouped(t.Prompt), grouped(t.Completion), rateText(t), costText(t))
}

func usageHead(b *strings.Builder, w int, first string) {
	fmt.Fprintf(b, "  %-*s %8s %12s %11s %6s  %s\n", w, first, "requests", "processed", "generated", "cache", "cost")
}

func renderUsage(r usageReport) string {
	var b strings.Builder
	w := 12
	for k := range r.ByModel {
		w = max(w, len(k))
	}
	b.WriteString("dun usage")
	if !r.Since.IsZero() {
		b.WriteString(" (since " + r.Since.Local().Format("2006-01-02") + ")")
	}
	b.WriteString("\n\n")
	usageHead(&b, w, "period")
	for _, p := range r.Periods {
		usageRow(&b, w, p.Name, p.usageTotals)
	}
	if len(r.ByModel) > 0 {
		b.WriteString("\n")
		usageHead(&b, w, "model")
		keys := make([]string, 0, len(r.ByModel))
		for k := range r.ByModel {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, c := r.ByModel[keys[i]], r.ByModel[keys[j]]
			if a.Prompt+a.Completion != c.Prompt+c.Completion {
				return a.Prompt+a.Completion > c.Prompt+c.Completion
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			usageRow(&b, w, k, *r.ByModel[k])
		}
	}
	if len(r.ByRole) > 0 {
		b.WriteString("\n")
		usageHead(&b, w, "agent role")
		for _, k := range []string{"main", "model", "vision", "(unrecorded)"} {
			if t := r.ByRole[k]; t != nil {
				usageRow(&b, w, k, *t)
			}
		}
		for _, k := range []string{"main", "model", "vision"} {
			if r.ByRole[k] != nil {
				fmt.Fprintf(&b, "    %s: %s\n", k, roleNote[k])
			}
		}
	}
	b.WriteString("\nleft: ")
	bd := r.Budget
	if bd.Status != "reported" {
		b.WriteString("not known: " + bd.Detail + "\n")
	} else {
		var parts []string
		if bd.Remaining != nil {
			parts = append(parts, fmt.Sprintf("%.4f %s remaining", *bd.Remaining, bd.Currency))
		}
		if bd.Limit != nil {
			s := fmt.Sprintf("of %.4f", *bd.Limit)
			if bd.Remaining != nil && *bd.Limit > 0 {
				s += fmt.Sprintf(" (%.0f%%)", 100**bd.Remaining / *bd.Limit)
			}
			parts = append(parts, s)
		}
		if bd.Remaining == nil && bd.Limit == nil {
			parts = append(parts, "no limit set")
		}
		parts = append(parts, fmt.Sprintf("; key spent %.4f", bd.Spend))
		if bd.Interval != "" {
			parts = append(parts, "per "+bd.Interval)
		}
		if bd.ResetAt != "" {
			parts = append(parts, "resets "+bd.ResetAt)
		}
		b.WriteString(strings.Join(parts, " ") + "\n")
	}
	for _, n := range r.Notes {
		b.WriteString("note: " + n + "\n")
	}
	return b.String()
}
