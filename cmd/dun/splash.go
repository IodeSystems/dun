package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The splash: what a person acts on in this session — the folder, the
// model, the servers, the extensions, the skills, the context files, what
// the context holds before they type, and what has been spent — at the top
// of a fresh session's conversation. /status says the same and the detail
// (the build, the tool count, each server's, skill's and file's share, the
// spend by period and by model), at the end of the conversation, as of when
// it was asked.
//
// Both are client-side entries like every line the TUI writes itself
// (m.append): the engine never sees them, so they are not in the session log
// and never sent to the model, and a resume does not replay them. A fresh
// session shows the splash; a resumed one does not (its history is the top of
// the screen).
//
// The splash is LIVE: refresh draws it again from the state as it is now (a
// server added, started or stopped; a model picked; the exact count landing
// after the estimate), so it never says what is no longer true. It is always
// the same number of rows (splashRows), each fitted to the width and ending
// at a word, so a change rewrites a row in place and nothing below it moves.

// statusSection is one row of the splash: a label in the label column and
// its value, already styled. more is a second row in the value column ("" is
// no second row; a section with more always has it, so its height is fixed).
type statusSection struct {
	label, value string
	more         *string
}

// splashRows is how many rows the splash's sections are, whatever they say:
// eight sections, the context one two rows.
const splashRows = 9

// builtinServer are the servers that come with dun and are not optional
// extensions: they are on the servers row even though they have a switch.
var builtinServer = map[string]bool{"poly-lsp": true}

// contextHeavy is the share of the model's window, loaded before the person
// types, from which the context row is orange: past it, most of the window
// is gone before the conversation starts.
const contextHeavy = 0.60

// splashSections are the splash's rows, in order: always the same ones.
// w is the row width; each value is fitted to what is left of it.
func (m tuiModel) splashSections(w int) []statusSection {
	vw := w - splashValueCol
	ctx, parts := m.contextValue(vw)
	return []statusSection{
		{label: "folder", value: tildePath(m.workspace)},
		{label: "model", value: m.modelValue()},
		{label: "servers", value: m.serversValue(vw)},
		{label: "extensions", value: m.extensionsValue(vw)},
		{label: "skills", value: m.skillsValue()},
		{label: "context files", value: m.filesValue()},
		{label: "context", value: ctx, more: &parts},
		{label: "spend", value: m.spendValue(vw, time.Now())},
	}
}

// splashLabelW is the label column's width ("context files"), and
// splashValueCol where the values start: two spaces, the label, two spaces.
const (
	splashLabelW   = len("context files")
	splashValueCol = 2 + splashLabelW + 2
)

// statusSections are /status's rows: the splash's, with the build and the
// tool count.
func (m tuiModel) statusSections(w int) []statusSection {
	secs := m.splashSections(w)
	out := []statusSection{{label: "build", value: version}, secs[0], secs[1]}
	out = append(out, statusSection{label: "tools", value: fmt.Sprintf("%d", len(m.tools))})
	return append(out, secs[2:]...)
}

func (m tuiModel) modelValue() string {
	switch {
	case m.model != "" && m.url != "":
		v := m.model
		if m.provider != "" {
			v += stDim.Render(" on ") + m.provider
		}
		return v
	case m.url != "":
		return stDim.Render("none yet — /model")
	}
	return stDim.Render("none yet")
}

// serversValue is every server that is not an optional extension — the
// built-in ones (poly-lsp) included: starting (for how long), failed,
// running (with its tools), or off, in that order, so what is changing is
// what a narrow row keeps. Narrow, the tool counts go first.
func (m tuiModel) serversValue(w int) string {
	var short, shortest, long []string
	for _, st := range sortedServers(m.servers) {
		if isExtension(st) && st.Err == "" {
			continue // the extensions row
		}
		s := st.ID + " "
		var tools string
		on := s // the phone's form: "on" for running
		switch {
		case st.Starting:
			s += stDim.Render("starting" + m.startedFor(st.ID))
			on = s
		case st.Err != "":
			s += stErr.Render("did not start")
			on = s
		case st.Running:
			s += stOK.Render("running")
			on += stOK.Render("on")
			if st.Tools > 0 {
				tools = stDim.Render(" · " + plural(st.Tools, "tool"))
			}
		default:
			s += stDim.Render("off")
			on = s
		}
		short, shortest, long = append(short, s), append(shortest, on), append(long, s+tools)
	}
	if len(long) == 0 {
		return stDim.Render("none yet")
	}
	order := func(rows []string) string {
		// Stable by the rank of the state each row says: starting, failed,
		// running, off.
		rank := func(r string) int {
			p := ansi.Strip(r)
			switch {
			case strings.Contains(p, " starting"):
				return 0
			case strings.Contains(p, " did not start"):
				return 1
			case strings.Contains(p, " running"), strings.HasSuffix(p, " on"):
				return 2
			}
			return 3
		}
		rows = slices.Clone(rows)
		sort.SliceStable(rows, func(a, b int) bool { return rank(rows[a]) < rank(rows[b]) })
		return strings.Join(rows, stDim.Render(" · "))
	}
	return firstFit(w, order(long), order(short), order(shortest))
}

// isExtension is a server the extensions row is for: one with a switch
// (/raglit) that is not a built-in server.
func isExtension(st dun.ServerState) bool { return st.Slash != "" && !builtinServer[st.ID] }

// startedFor is " 12s": how long a starting server has been at it, on this
// clock (noteStarting), or "…" when the TUI has not seen it start.
func (m tuiModel) startedFor(id string) string {
	if t, ok := m.srvSince[id]; ok {
		return fmt.Sprintf(" %ds", int(time.Since(t).Seconds()))
	}
	return "…"
}

func sortedServers(states []dun.ServerState) []dun.ServerState {
	sorted := slices.Clone(states)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	return sorted
}

// extensionsValue is the installed extensions, each on or off, and where to
// read about the first one that is off:
//
//	raglit off · /help raglit
func (m tuiModel) extensionsValue(w int) string {
	var parts []string
	first := ""
	for _, st := range sortedServers(m.servers) {
		if !isExtension(st) || st.Err != "" {
			continue
		}
		switch {
		case st.Starting:
			parts = append(parts, st.ID+stDim.Render(" starting"+m.startedFor(st.ID)))
		case st.Running:
			parts = append(parts, st.ID+" "+stOK.Render("on"))
		default:
			parts = append(parts, st.ID+stDim.Render(" off"))
			if first == "" {
				first = st.Slash
			}
		}
	}
	if len(parts) == 0 {
		return stDim.Render("none")
	}
	v := strings.Join(parts, stDim.Render(" · "))
	if first != "" {
		if more := stDim.Render(" · /help " + first); ansi.StringWidth(v+more) <= w {
			v += more
		}
	}
	return v
}

// skillsValue is how many skills the agent can load, and the command that
// lists them.
func (m tuiModel) skillsValue() string {
	if len(m.skills) == 0 {
		return stDim.Render("none")
	}
	return fmt.Sprintf("%d", len(m.skills)) + stDim.Render(" · /skill")
}

// filesValue names the context files the agent reads (rules.go: AGENTS.md,
// CLAUDE.md and what they import), in reading order.
func (m tuiModel) filesValue() string {
	s := m.ctxStats
	if s.systemTokens == 0 {
		return stDim.Render("counting…")
	}
	if len(s.systemFiles) == 0 {
		return stDim.Render("none")
	}
	names := make([]string, len(s.systemFiles))
	for i, f := range s.systemFiles {
		names[i] = f.Name
	}
	return strings.Join(names, stDim.Render(", "))
}

// contextValue is the context row: what the context holds before the person
// types, of the model's window when it is known, and its parts on the second
// row. "~" marks an estimate; the exact count, when the provider counts,
// replaces it in place. Orange from contextHeavy of the window.
//
//	~18.4k of 32k before you type (57%)
//	system 2.1k · tools 6.8k (mcp 5.2k) · skills 4.0k · context files 5.5k
func (m tuiModel) contextValue(w int) (string, string) {
	s := m.ctxStats
	if s.systemTokens == 0 {
		return stDim.Render("counting…"), ""
	}
	n := tokK(s.systemTokens)
	if !s.systemExact {
		n = "~" + n
	}
	var v string
	heavy := false
	if s.window > 0 {
		pct := float64(s.systemTokens) / float64(s.window)
		heavy = pct >= contextHeavy
		p := fmt.Sprintf("(%d%%)", int(math.Round(pct*100)))
		v = firstFit(w, n+" of "+tokK(s.window)+" before you type "+p, n+" of "+tokK(s.window)+" "+p)
	} else {
		v = firstFit(w, n+" before you type", n)
	}
	if heavy {
		v = stNote.Render(v)
	}
	// The parts may take the whole row under the label when the value
	// column is too narrow for them (sectionsView).
	return v, stSecondary.Render(m.contextParts(w + splashValueCol - 2))
}

// contextParts is the context's parts in words, the longest form that fits
// w, then cut at a word: the system prompt (without the context files), the
// tools (with the MCP servers' share), the skill list and the context files.
func (m tuiModel) contextParts(w int) string {
	s := m.ctxStats
	var builtin, mcp, skills int
	for _, p := range s.systemParts {
		switch {
		case p.Name == dun.SkillsPart:
			skills += p.Tokens
		case strings.HasPrefix(p.Name, "mcp: "):
			mcp += p.Tokens
		default:
			builtin += p.Tokens
		}
	}
	system := max(0, s.systemPrompt-s.systemRules)
	tools := builtin + mcp + s.systemShared
	form := func(sys, files string, withMCP bool) string {
		out := []string{sys + " " + tokK(system)}
		t := "tools " + tokK(tools)
		if withMCP && mcp > 0 {
			t += " (mcp " + tokK(mcp) + ")"
		}
		out = append(out, t)
		if skills > 0 {
			out = append(out, "skills "+tokK(skills))
		}
		if s.systemRules > 0 {
			out = append(out, files+" "+tokK(s.systemRules))
		}
		return strings.Join(out, " · ")
	}
	return fitText(firstFit(w, form("system", "context files", true), form("system", "files", true),
		form("system", "files", false), form("sys", "files", false)), w)
}

// firstFit is the first of forms that fits w cells, else the last.
func firstFit(w int, forms ...string) string {
	for _, f := range forms {
		if ansi.StringWidth(f) <= w {
			return f
		}
	}
	return forms[len(forms)-1]
}

// tokK is a token count the short way: 940, 18.4k, 128k, 1.05M.
func tokK(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.2f", float64(n)/1e6), ".00") + "M"
}

// spendValue is the spend row: the smallest period whose spend is
// significant (spendPeriod), then what the key has left when the gateway
// says (orange when it covers a day or less at the key's rate over the last
// 24 hours). A provider that reports no cost is "not reported by <host>",
// never $0.
//
//	$0.42 today · $12.30 left on work
func (m tuiModel) spendValue(w int, now time.Time) string {
	sp := m.ctxStats.spend
	host := hostName(m.url)
	unpriced := sp != nil && sp.Calls > 0 && sp.CostedCalls == 0
	var v string
	if sp != nil && sp.Ledger != nil {
		if amt, period, ok := spendPeriod(sp.Ledger); ok {
			v = amt + " " + period
		} else if sp.Ledger.Entries {
			v = "< $0.01 this year"
		}
	}
	switch {
	case v == "" && unpriced:
		return stDim.Render("not reported by " + host)
	case v == "":
		return stDim.Render("none yet")
	case unpriced:
		v += stDim.Render(" · not reported by " + host)
	}
	left, low, ok := budgetLeft(sp)
	if !ok {
		return v
	}
	tail := left + " left"
	if m.provider != "" && ansi.StringWidth(v+" · "+tail+" on "+m.provider) <= w {
		tail += " on " + m.provider
	}
	if low {
		tail = stNote.Render(tail)
	}
	return v + stDim.Render(" · ") + tail
}

// spendPeriods are the periods the spend row picks from, smallest first.
var spendPeriods = []string{"today", "this week", "this month", "this year"}

// spendPeriod is the smallest period whose spend is significant — $0.01, or
// one credit — and its amount in words.
func spendPeriod(l *proto.SpendLedger) (string, string, bool) {
	for i, by := range []map[string]float64{l.Today, l.Week, l.Month, l.Year} {
		if significant(by) {
			return moneyIn(by), spendPeriods[i], true
		}
	}
	return "", "", false
}

// significant: any currency's amount reaches its floor (one credit; a cent
// of anything else).
func significant(by map[string]float64) bool {
	for cur, v := range by {
		floor := 0.01
		if cur == "credits" {
			floor = 1
		}
		if v >= floor-1e-9 {
			return true
		}
	}
	return false
}

// moneyIn is amounts by currency in words: "$0.42", "3.2 credits", both
// joined with " + ".
func moneyIn(by map[string]float64) string {
	curs := make([]string, 0, len(by))
	for c, v := range by {
		if v > 0 {
			curs = append(curs, c)
		}
	}
	sort.Strings(curs)
	out := make([]string, 0, len(curs))
	for _, c := range curs {
		out = append(out, moneyOf(by[c], c))
	}
	return strings.Join(out, " + ")
}

// moneyOf is one amount in its currency.
func moneyOf(v float64, cur string) string {
	switch cur {
	case "USD":
		return "$" + money(v)
	case "credits":
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + " credits"
	case "":
		return money(v)
	}
	return money(v) + " " + cur
}

// budgetLeft is what the key has left, in its currency, and whether that is
// a day or less at the key's spend over the last 24 hours (or nothing).
func budgetLeft(sp *proto.Spend) (string, bool, bool) {
	if sp == nil || sp.Key == nil {
		return "", false, false
	}
	k := sp.Key
	var left float64
	switch {
	case k.Remaining != nil:
		left = *k.Remaining
	case k.Limit != nil:
		left = *k.Limit - k.Spend
	default:
		return "", false, false
	}
	rate := 0.0
	if sp.Ledger != nil {
		rate = sp.Ledger.Last24h[k.Currency]
	}
	low := left <= 0 || (rate > 0 && left <= rate)
	return moneyOf(max(0, left), k.Currency), low, true
}

// sectionsView is rows of label + value — a label column (dim) and a value
// column, each row fitted to w cells so it stays one row, a section's
// second row in the value column — then a blank row that sets the block
// apart from what follows.
func sectionsView(secs []statusSection, w int) string {
	lw := 0
	for _, s := range secs {
		lw = max(lw, len(s.label))
	}
	rows := make([]string, 0, len(secs)+1)
	for _, s := range secs {
		rows = append(rows, fitText("  "+stDim.Render(fmt.Sprintf("%-*s", lw, s.label))+"  "+s.value, w))
		if s.more != nil {
			// In the value column; on a phone, where the column is too narrow
			// for it, from the label column.
			indent := lw + 4
			if ansi.StringWidth(*s.more) > w-indent {
				indent = 2
			}
			rows = append(rows, fitText(strings.Repeat(" ", indent)+*s.more, w))
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// splashView is the splash as the state is now, at width w.
func (m tuiModel) splashView(w int) string { return sectionsView(m.splashSections(w), w) }

// splashW is the width the splash's rows are cut to: one column less than
// the pane, so the selection gutter never wraps one.
func (m tuiModel) splashW() int { return max(20, m.vp.Width-1) }

// showSplash puts the splash at the top of the conversation: a fresh
// session's first entry, drawn again from live state at every refresh.
func (m *tuiModel) showSplash() {
	m.convo = slices.Insert(m.convo, 0, convoEntry{collapsed: m.splashView(m.splashW()), splash: true, liveSplash: true})
	m.refresh()
}

// liveSplashNow brings the live splash up to date; refresh calls it before
// it draws. A changed row is the same row with new words.
func (m *tuiModel) liveSplashNow() {
	if len(m.convo) == 0 || !m.convo[0].liveSplash {
		return
	}
	if v := m.splashView(m.splashW()); v != m.convo[0].collapsed {
		m.convo[0].collapsed = v
		m.convo[0].invalidateWrap()
	}
}

// statusSlash is /status: everything, as of now, at the end of the
// conversation — the splash's rows, then each server's, skill's and context
// file's share of the context, and the spend by period, by model and for
// this session.
func (m *tuiModel) statusSlash() {
	w := m.splashW()
	text := sectionsView(m.statusSections(w), w) + m.statusDetail(w)
	m.convo = append(m.convo, convoEntry{collapsed: text, splash: true})
	m.refresh()
}

// statusDetail is /status's detail blocks, each a heading and rows of name
// and value, every row fitted to w.
func (m tuiModel) statusDetail(w int) string {
	s := m.ctxStats
	est := "~"
	if s.systemExact {
		est = ""
	}
	var b strings.Builder
	block := func(title string, rows [][2]string) {
		if len(rows) == 0 {
			return
		}
		b.WriteString("\n  " + stHeader.Render(title) + "\n")
		nw := 0
		for _, r := range rows {
			nw = max(nw, ansi.StringWidth(r[0]))
		}
		nw = min(nw, max(8, w/2))
		for _, r := range rows {
			name := fitText(r[0], nw)
			b.WriteString(fitText("    "+name+strings.Repeat(" ", max(0, nw-ansi.StringWidth(name)))+"  "+r[1], w) + "\n")
		}
	}
	// Servers: their tools' share of the context.
	var srv [][2]string
	tokensOf := map[string]int{}
	for _, p := range s.systemParts {
		if id, ok := strings.CutPrefix(p.Name, "mcp: "); ok {
			tokensOf[id] = p.Tokens
		}
	}
	for _, st := range sortedServers(m.servers) {
		state := stDim.Render("off")
		switch {
		case st.Starting:
			state = stDim.Render("starting" + m.startedFor(st.ID))
		case st.Running:
			state = stOK.Render("running") + stDim.Render(" · "+plural(st.Tools, "tool"))
		case st.Err != "":
			state = stErr.Render("did not start")
		}
		if n, ok := tokensOf[st.ID]; ok && st.Running {
			state += stDim.Render(" · ") + est + tokK(n)
		}
		srv = append(srv, [2]string{st.ID, state})
	}
	block("servers", srv)
	// Skills: each one's line in the skill list, estimated.
	var sk [][2]string
	for _, k := range m.skills {
		v := "~" + tokK(estimateTok("\n- "+k.Name+": "+k.Description))
		if k.UserOnly {
			v = stDim.Render("/skill only")
		}
		sk = append(sk, [2]string{k.Name, v})
	}
	block("skills", sk)
	var fl [][2]string
	for _, f := range s.systemFiles {
		fl = append(fl, [2]string{f.Name, "~" + tokK(f.Tokens)})
	}
	block("context files", fl)
	block("spend", m.spendRows())
	return b.String()
}

// estimateTok is the four-characters-per-token estimate the engine uses
// when it cannot count (dun/context_tokens.go).
func estimateTok(s string) int { return len(s) / 4 }

// spendRows are /status's spend rows: today, this week, this month, this
// year, this month by model, this session, and the key's budget.
func (m tuiModel) spendRows() [][2]string {
	sp := m.ctxStats.spend
	host := hostName(m.url)
	var rows [][2]string
	amt := func(by map[string]float64) string {
		if len(by) == 0 || moneyIn(by) == "" {
			return stDim.Render("none")
		}
		return moneyIn(by)
	}
	if sp != nil && sp.Ledger != nil {
		l := sp.Ledger
		rows = append(rows, [2]string{"today", amt(l.Today)}, [2]string{"this week", amt(l.Week)},
			[2]string{"this month", amt(l.Month)}, [2]string{"this year", amt(l.Year)})
		models := make([]string, 0, len(l.ByModel))
		for k := range l.ByModel {
			models = append(models, k)
		}
		sort.Strings(models)
		for _, k := range models {
			rows = append(rows, [2]string{k, amt(l.ByModel[k]) + stDim.Render(" this month")})
		}
	}
	switch {
	case sp == nil || sp.Calls == 0:
		rows = append(rows, [2]string{"this session", stDim.Render("none yet")})
	case sp.Session == nil:
		rows = append(rows, [2]string{"this session", stDim.Render(fmt.Sprintf("not reported by %s (%s)", host, plural(sp.Calls, "call")))})
	default:
		rows = append(rows, [2]string{"this session", moneyOf(*sp.Session, sp.Currency) + stDim.Render(" · "+plural(sp.CostedCalls, "priced call"))})
	}
	if left, low, ok := budgetLeft(sp); ok {
		v := left + " left"
		if low {
			v = stNote.Render(v + " · a day or less at the last 24 hours' rate")
		}
		name := "key budget"
		if m.provider != "" {
			name = m.provider + " key"
		}
		rows = append(rows, [2]string{name, v})
	}
	return rows
}
