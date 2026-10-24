package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
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
// its value, already styled. more are rows under it in the value column (the
// context's parts); how many depends on the width only, so a section's
// height never changes with what it says.
type statusSection struct {
	label, value string
	more         []string
}

// builtinServer are the servers that come with dun and are not optional
// extensions: they are on the servers row even though they have a switch.
var builtinServer = map[string]bool{"poly-lsp": true}

// contextHeavy is the share of the model's window, loaded before the person
// types, from which the context row is orange: past it, most of the window
// is gone before the conversation starts.
const contextHeavy = 0.60

// splashSections are the splash's rows, in order: always the same ones.
// w is the row width; each value is fitted to what is left of it. short is
// the phone beside the foreman: "files" for "context files", three columns
// more for every value.
func (m tuiModel) splashSections(w int, short bool) []statusSection {
	vw, files := w-splashValueCol, "context files"
	if short {
		vw, files = w-(2+len("extensions")+2), "files"
	}
	ctx, parts := m.contextValue(vw)
	return []statusSection{
		{label: "folder", value: tildePath(m.workspace)},
		{label: "model", value: m.modelValue()},
		{label: "tools", value: m.toolsValue(vw)},
		{label: "extensions", value: m.extensionsValue(vw)},
		{label: "skills", value: m.skillsValue()},
		{label: files, value: m.filesValue()},
		{label: "context", value: ctx, more: parts},
		{label: "spend", value: m.spendValue(vw, time.Now())},
	}
}

// splashLabelW is the label column's width ("context files"), and
// splashValueCol where the values start: two spaces, the label, two spaces.
const (
	splashLabelW   = len("context files")
	splashValueCol = 2 + splashLabelW + 2
)

// statusSections are /status's rows: the splash's, with the build.
func (m tuiModel) statusSections(w int) []statusSection {
	secs := m.splashSections(w, false)
	out := []statusSection{{label: "build", value: version}}
	return append(out, secs...)
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

// toolsValue is how many tools the model has — dun's own and every
// server's — what their definitions cost in every request, and /mcp info for
// where they come from (USER 2026-10-24: "mcp should probably be just tools";
// "5 (36k) for 5 tool definitions"). A server that is
// starting or did not start is named: the one thing on the row that is
// changing, or wrong. Narrow, the hint goes first.
func (m tuiModel) toolsValue(w int) string {
	var trouble []string
	for _, st := range m.splashServers() {
		if isExtension(st) && st.Err == "" {
			continue // the extensions row
		}
		switch {
		case st.Starting:
			trouble = append(trouble, st.ID+stDim.Render(" starting"+m.startedFor(st.ID)))
		case st.Missing: // a setup step, not a failure: the line under the splash says it
			trouble = append(trouble, st.ID+stDim.Render(" not installed"))
		case st.Err != "":
			trouble = append(trouble, st.ID+" "+stErr.Render("did not start"))
		case st.Running && st.Index == dun.IndexBuilding:
			trouble = append(trouble, st.ID+stDim.Render(" indexing"+m.indexingFor(st.ID)))
		case st.Running && st.Index == dun.IndexFailed:
			trouble = append(trouble, st.ID+" "+stErr.Render("index failed"))
		}
	}
	sep := stDim.Render(" · ")
	head := fmt.Sprintf("%d", len(m.tools))
	if tok := m.toolTokens(); tok > 0 {
		head += stDim.Render(" (" + tokN(tok) + ")")
	}
	for _, t := range trouble {
		head += sep + t
	}
	return firstFit(w, head+sep+stDim.Render("/mcp info"), head)
}

// splashServers are the servers in the order this TUI first saw them: a
// server added later goes at the end, and a state changing rewrites a
// server's words where they are — nothing on the row moves over (run 5:
// "notes" sorted before "shell" and pushed it right).
func (m tuiModel) splashServers() []dun.ServerState {
	out := sortedServers(m.servers)
	at := func(id string) int {
		if i := slices.Index(m.srvOrder, id); i >= 0 {
			return i
		}
		return len(m.srvOrder)
	}
	sort.SliceStable(out, func(a, b int) bool { return at(out[a].ID) < at(out[b].ID) })
	return out
}

// noteServerOrder appends the servers this TUI has not seen before to
// srvOrder: those seen together by what they are doing — starting, failed,
// running, off — so a narrow row starts with what works; then by name. A
// new slice: model copies share the old.
func (m *tuiModel) noteServerOrder() {
	var fresh []dun.ServerState
	for _, st := range sortedServers(m.servers) {
		if !slices.Contains(m.srvOrder, st.ID) {
			fresh = append(fresh, st)
		}
	}
	rank := func(st dun.ServerState) int {
		switch {
		case st.Starting:
			return 0
		case st.Err != "":
			return 1
		case st.Running:
			return 2
		}
		return 3
	}
	sort.SliceStable(fresh, func(a, b int) bool { return rank(fresh[a]) < rank(fresh[b]) })
	next := slices.Clone(m.srvOrder)
	for _, st := range fresh {
		next = append(next, st.ID)
	}
	m.srvOrder = next
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

// extensionsValue is how many extensions are installed and how many are on,
// any still starting by name, and /ext info for the list:
//
//	1 of 2 on · /ext info
func (m tuiModel) extensionsValue(w int) string {
	var n, on int
	var trouble []string
	for _, st := range m.splashServers() {
		if !isExtension(st) || st.Err != "" {
			continue
		}
		n++
		switch {
		case st.Starting:
			trouble = append(trouble, st.ID+stDim.Render(" starting"+m.startedFor(st.ID)))
		case st.Running:
			on++
		}
	}
	if n == 0 {
		return stDim.Render("none")
	}
	sep := stDim.Render(" · ")
	head := fmt.Sprintf("%d of %d on", on, n)
	for _, t := range trouble {
		head += sep + t
	}
	return firstFit(w, head+sep+stDim.Render("/ext info"), head)
}

// skillsValue is how many skills the agent can load, and the command that
// lists them.
func (m tuiModel) skillsValue() string {
	if len(m.skills) == 0 {
		return stDim.Render("none")
	}
	v := fmt.Sprintf("%d", len(m.skills))
	if tok := m.skillTokens(); tok > 0 {
		v += stDim.Render(" (" + tokN(tok) + ")")
	}
	return v + stDim.Render(" · /skill info")
}

// toolTokens is what every tool definition costs in each request — dun's own
// with the provider's tool-use overhead, and every server's; 0 while
// counting.
func (m tuiModel) toolTokens() int {
	s := m.ctxStats
	if s.systemTokens == 0 {
		return 0
	}
	n := s.systemShared
	for _, p := range s.systemParts {
		if p.Name != dun.SkillsPart {
			n += p.Tokens
		}
	}
	return n
}

// skillTokens is what the skill list costs in each request; 0 while counting.
func (m tuiModel) skillTokens() int {
	n := 0
	for _, p := range m.ctxStats.systemParts {
		if p.Name == dun.SkillsPart {
			n += p.Tokens
		}
	}
	return n
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
// rows. "~" marks an estimate; the exact count, when the provider counts,
// replaces it in place. Orange from contextHeavy of the window. Every count
// in tokens (tokN), so one row reads in one precision.
//
//	~18.4k tokens before you type (of 32k, 57%)
//	system 2.1k · tools 16 (4.8k) · mcp 1 server (344) · skills 14 (2.1k) · context files 0 (0)
func (m tuiModel) contextValue(w int) (string, []string) {
	s := m.ctxStats
	rows := partsRows(w)
	if s.systemTokens == 0 {
		return stDim.Render("counting…"), make([]string, rows)
	}
	n := tokN(s.systemTokens)
	if !s.systemExact {
		n = "~" + n
	}
	var v string
	heavy := false
	if s.window > 0 {
		pct := float64(s.systemTokens) / float64(s.window)
		heavy = pct >= contextHeavy
		of := fmt.Sprintf("of %s, %d%%", tokK(s.window), int(math.Round(pct*100)))
		v = firstFit(w, n+" tokens before you type ("+of+")", n+" tokens ("+of+")", n+" ("+of+")",
			fmt.Sprintf("%s (%d%%)", n, int(math.Round(pct*100))))
	} else {
		v = firstFit(w, n+" tokens before you type", n+" tokens", n)
	}
	if heavy {
		v = stNote.Render(v)
	}
	parts := m.contextParts(w, rows)
	for i := range parts {
		parts[i] = stSecondary.Render(parts[i])
	}
	return v, parts
}

// partsTemplate is the context's parts at their widest likely numbers: a
// width that fits it fits the parts on one row whatever they say, so the
// row count is the width's alone and a count changing never adds a row.
const partsTemplate = "system 99.9k · tools 99 (99.9k) · mcp 9 servers (99.9k) · skills 99 (99.9k) · context files 99 (99.9k)"

// partsRows is how many rows the context's parts take in a value column w
// cells wide: one when the template fits, else two.
func partsRows(w int) int {
	if len(partsTemplate) <= w {
		return 1
	}
	return 2
}

// contextParts is the context's parts in words, each its count and its
// tokens, zeros included: the system prompt (without the context files),
// dun's own tools (with the provider's tool-use overhead), the MCP servers'
// tools, the skill list and the context files. On rows rows of w cells: two
// rows put the skills and the files on the second; a row too long for w
// ends at a word.
func (m tuiModel) contextParts(w, rows int) []string {
	s := m.ctxStats
	var builtin, mcp, skills, servers int
	for _, p := range s.systemParts {
		switch {
		case p.Name == dun.SkillsPart:
			skills += p.Tokens
		case strings.HasPrefix(p.Name, "mcp: "):
			mcp += p.Tokens
			servers++
		default:
			builtin += p.Tokens
		}
	}
	system := max(0, s.systemPrompt-s.systemRules)
	part := func(name string, n, tok int) string { return fmt.Sprintf("%s %d (%s)", name, n, tokN(tok)) }
	items := []string{
		"system " + tokN(system),
		part("tools", m.builtinToolCount(), builtin+s.systemShared),
		fmt.Sprintf("mcp %s (%s)", plural(servers, "server"), tokN(mcp)),
		part("skills", len(m.skills), skills),
		part("context files", len(s.systemFiles), s.systemRules),
	}
	short := slices.Clone(items)
	short[4] = part("files", len(s.systemFiles), s.systemRules)
	if rows == 1 {
		return []string{fitText(firstFit(w, strings.Join(items, " · "), strings.Join(short, " · ")), w)}
	}
	return []string{
		fitText(strings.Join(items[:3], " · "), w),
		fitText(firstFit(w, strings.Join(items[3:], " · "), strings.Join(short[3:], " · ")), w),
	}
}

// builtinToolCount is how many of the tools are dun's own: all of them less
// the skill tool and the running MCP servers' (a server hosted in the shell
// lists its tools under the shell's).
func (m tuiModel) builtinToolCount() int {
	n := len(m.tools)
	if slices.Contains(m.tools, "skill") {
		n--
	}
	for _, st := range m.servers {
		if st.Running && st.Via != dun.ViaShell {
			n -= st.Tools
		}
	}
	return max(0, n)
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

// tokN is a token count in one precision: plain below 1000, then thousands
// with one decimal (1.0k, 18.4k, 128.3k), then millions with two.
func tokN(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1e6)
}

// tokK is a size the short way — a window: 940, 18.4k, 128k, 1.05M.
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
// 24 hours). A provider that reports no cost is "not reported by <service>",
// never $0.
//
//	$0.42 today · $12.30 left on work
func (m tuiModel) spendValue(w int, now time.Time) string {
	sp := m.ctxStats.spend
	host := m.serviceName()
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
	case v == "":
		// No spend and no budget: the row is left blank, not filled with
		// "none yet" (USER 2026-10-24); its place is kept, so spend arriving
		// later fills it without the splash growing.
		return ""
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
		if s.value == "" && len(s.more) == 0 {
			rows = append(rows, "") // a row with nothing to say keeps its place
			continue
		}
		rows = append(rows, fitText("  "+stDim.Render(fmt.Sprintf("%-*s", lw, s.label))+"  "+s.value, w))
		// In the value column at every width, under the value it belongs to
		// (at 49 columns it started under the label and read as a row of
		// its own: run 5).
		for _, more := range s.more {
			rows = append(rows, fitText(strings.Repeat(" ", lw+4)+more, w))
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// splashView is the splash as the state is now, at width w: the tagline
// row, then the sections, with the foreman (mascot.go) to their left when
// the terminal can draw him. He is never taller than the rows beside him
// (the rows are at least his height; when they are not, blank rows make
// them so), so the splash's height is the width's alone.
func (m tuiModel) splashView(w int) string {
	grid, tag := mascotFor(m.w)
	tagRow := lipgloss.NewStyle().Bold(true).Render("dun") + stDim.Render(" · ") + stSecondary.Render(tag)
	if grid == nil {
		return fitText("  "+tagRow, w) + "\n" + sectionsView(m.splashSections(w, false), w)
	}
	art, aw := mascotArt(grid), mascotWidth(grid)
	rw := max(20, w-aw)
	body := strings.Split(strings.TrimSuffix(sectionsView(m.splashSections(rw, m.w < mascotNarrowW), rw), "\n"), "\n")
	rows := append([]string{fitText("  "+tagRow, rw)}, body...)
	out := make([]string, max(len(rows), len(art)))
	for i := range out {
		a := strings.Repeat(" ", aw)
		if i < len(art) {
			a = art[i]
		}
		r := ""
		if i < len(rows) {
			r = rows[i]
		}
		out[i] = a + r
	}
	return strings.Join(out, "\n") + "\n"
}

// splashModelRow is the live splash's model row: under the tagline and the
// folder.
const splashModelRow = 2

// splashModelShown reports whether the live splash's model row is in the
// view: the header leaves the model out then (run 5: the model three times
// on one screen — header, splash, notice). Scrolled past it, the header
// says it again, so the model is always on screen.
func (m tuiModel) splashModelShown() bool {
	if len(m.convo) == 0 || !m.convo[0].liveSplash {
		return false
	}
	top := 0
	if len(m.frame.rowOffset) > 0 {
		top = m.frame.rowOffset[0]
	}
	row := top + splashModelRow
	return row >= m.vp.YOffset && row < m.vp.YOffset+m.vp.Height
}

// serviceName is the AI service as the person named it at /login ("local",
// "work"), or its host when the session has no saved name: a row says the
// name the person knows, never an address (run 5: "not reported by
// 127.0.0.1").
func (m tuiModel) serviceName() string {
	if m.provider != "" {
		return m.provider
	}
	return hostName(m.url)
}

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
		case st.Missing:
			state = stDim.Render("not installed")
		case st.Err != "":
			state = stErr.Render("did not start")
		}
		if n, ok := tokensOf[st.ID]; ok && st.Running {
			state += stDim.Render(" · ") + est + tokN(n)
		}
		srv = append(srv, [2]string{st.ID, state})
	}
	block("servers", srv)
	// Skills: each one's line in the skill list, estimated.
	var sk [][2]string
	for _, k := range m.skills {
		v := "~" + tokN(estimateTok("\n- "+k.Name+": "+k.Description))
		if k.UserOnly {
			v = stDim.Render("/skill only")
		}
		sk = append(sk, [2]string{k.Name, v})
	}
	block("skills", sk)
	var fl [][2]string
	for _, f := range s.systemFiles {
		fl = append(fl, [2]string{f.Name, "~" + tokN(f.Tokens)})
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
	host := m.serviceName()
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
