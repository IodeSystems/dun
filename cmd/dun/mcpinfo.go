package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/iodesystems/dun"
)

// /mcp info: every server — what it is doing, its mode, its tools and what
// they cost in every request — then the total and its share of the window,
// the shell (scripted mode) and what it costs, and for each standalone
// server heavy enough, the command that moves it into the shell with what
// that saves. The engine says it (control "mcpinfo"), so every client gets
// the same rows; they are plain and the conversation wraps them, so a narrow
// screen breaks a row between words and never cuts one.
//
// The numbers are the context breakdown's (context_tokens.go): a server's
// "mcp: <id>" part is MARGINAL — what leaving its schemas out saves — which
// is the question a savings hint answers. A scripted server has no part: its
// schemas are not in the request (mcpshell's eval description is fixed text),
// and what it does cost is its name in one sentence of the system prompt
// (dun.HostedLine), priced here at the engine's four characters a token.

// mcpInfoIn is what /mcp info reads.
type mcpInfoIn struct {
	servers []dun.ServerState
	parts   []dun.ContextPart // the context's parts; a server's is "mcp: <id>"
	counted bool              // the parts are in: the first count lands after the start
	exact   bool              // the parts were counted, not estimated
	window  int               // the model's window; 0 unknown
	// What the schemas cost over the stats ledger's recent rounds, and the
	// endpoint's measured prefill speed (dun/roundstats.go); zero when
	// nothing has been recorded or measured.
	usage       dun.StatsSummary
	usageDays   int
	speed       float64
	speedRounds int
}

// mcpInfo is /mcp info's rows.
func mcpInfo(in mcpInfoIn) string {
	est := "~"
	if in.exact {
		est = ""
	}
	cost, total := map[string]int{}, 0
	for _, p := range in.parts {
		if id, ok := strings.CutPrefix(p.Name, "mcp: "); ok {
			cost[id] = p.Tokens
			total += p.Tokens
		}
	}
	var shell *dun.ServerState
	idW := 0
	for i, st := range in.servers {
		idW = max(idW, len(st.ID))
		if st.ID == dun.ServerShell {
			shell = &in.servers[i]
		}
	}
	shellUp := shell != nil && shell.Running
	rows := []string{"mcp servers"}
	var scripted, hosted []string
	for _, st := range in.servers {
		f := []string{infoState(st, shellUp), infoMode(st.Via)}
		on := st.Running || (st.Via == dun.ViaShell && st.Auto && shellUp && st.Err == "")
		if on && st.Tools > 0 {
			f = append(f, fact(plural(st.Tools, "tool")))
		}
		switch {
		case !in.counted || !on:
		case st.Via == dun.ViaShell && st.Standalone > 0:
			f = append(f, fact("0 tokens")+" (standalone "+fact("~"+tokN(st.Standalone))+")")
		case st.Via == dun.ViaShell:
			f = append(f, fact("0 tokens"))
		default:
			f = append(f, fact(est+tokN(cost[st.ID])+" tokens"))
		}
		if st.Running && st.Setup != nil {
			if n := len(st.Setup.MissingLSPs()); n > 0 {
				f = append(f, fact(plural(n, "language server")+" missing")) // /poly-lsp lists them
			}
			if st.Setup.CapabilityOff(dun.CapSemantic) {
				f = append(f, fact("semantic search off")) // /raglit says why, /raglit setup turns it on
			}
		}
		rows = append(rows, "  "+pad(st.ID, idW)+"  "+strings.Join(f, " · "))
		if st.Via == dun.ViaShell {
			scripted = append(scripted, st.ID)
			if on {
				hosted = append(hosted, dun.ShellNamespace(st.ID)) // as hostedNamespaces names them
			}
		}
	}
	if len(in.servers) == 0 {
		rows = append(rows, "  none yet — /mcp add")
	}
	switch {
	case !in.counted:
		rows = append(rows, "  tokens: counting…")
	case in.window > 0:
		pct := int(math.Round(float64(total) * 100 / float64(in.window)))
		rows = append(rows, "  total "+fact(est+tokN(total)+" tokens")+" in every request, "+fact(fmt.Sprintf("%d%% of the %s window", pct, tokK(in.window))))
	default:
		rows = append(rows, "  total "+fact(est+tokN(total)+" tokens")+" in every request")
	}
	if in.counted {
		rows = append(rows, costRows(in, cost)...)
	}
	if shell != nil {
		rows = append(rows, shellRows(shell, shellUp, in, est, cost[dun.ServerShell], scripted, hosted)...)
	}
	if hints := scriptHints(in, cost, shellUp, hosted); len(hints) > 0 {
		rows = append(rows, "to save tokens:")
		rows = append(rows, hints...)
	}
	return strings.Join(append(rows, "/mcp help for more"), "\n")
}

// shellRows say what scripted mode is and what it costs: the shell's one
// eval tool, and the sentence that names the servers it hosts.
func shellRows(shell *dun.ServerState, shellUp bool, in mcpInfoIn, est string, evalTok int, scripted, hosted []string) []string {
	rows := []string{"scripted mode: the shell (mcpshell) hosts a server's tools in its one eval tool, called as <ns>.<tool>(…); " +
		"their schemas leave every request, and help(\"<ns>\") lists them when the model asks"}
	var cost string
	switch {
	case !shellUp:
		cost = "the shell is " + infoState(*shell, false) + " — /mcp on shell"
	case !in.counted:
		cost = "the shell's eval: counting…"
	default:
		cost = "the shell's eval: " + fact(est+tokN(evalTok)+" tokens")
		if len(hosted) > 0 {
			cost += " · the line naming its servers: " + fact("~"+tokN(estimateTok(dun.HostedLine(hosted)))+" tokens")
		}
	}
	if len(scripted) == 0 {
		cost += " · no server is scripted"
	} else {
		cost += " · scripted: " + strings.Join(scripted, ", ")
	}
	return append(rows, cost)
}

// scriptHints are the standalone servers worth moving into the shell, one
// row each, with what it saves: the server's marginal part, less what naming
// it in the hosted sentence adds. The shell's own eval is not counted
// against it — it is in the request already, or the row says to turn it on.
//
// The mark is dun.HeavyServerTokens (5000), the one /mcp tools draws its
// "heavy" hint at, so the two never disagree about which server is heavy.
// Under it the move is not worth what a scripted server gives up (docs
// indexing, prompts and skills, the tool deny-list, its call renderers —
// shellPlacementNote) and help() costs on every first use.
func scriptHints(in mcpInfoIn, cost map[string]int, shellUp bool, hosted []string) []string {
	if !in.counted {
		return nil
	}
	var out []string
	for _, st := range in.servers {
		n := cost[st.ID]
		// ViaDirect: an added server; a built-in has no mode. An oauth
		// server cannot be scripted — dun holds its token (checkOAuth).
		if st.Via != dun.ViaDirect || !st.Running || n < dun.HeavyServerTokens || isOAuth(st.Auth) {
			continue
		}
		after := append(slices.Clone(hosted), dun.ShellNamespace(st.ID))
		line := estimateTok(dun.HostedLine(after)) - estimateTok(dun.HostedLine(hosted))
		row := "  " + fact("/mcp mode "+st.ID+" scripted") + " — saves " + fact("~"+tokN(n-line)+" tokens")
		if !shellUp {
			row += ", with the shell on (/mcp on shell)"
		}
		out = append(out, row)
	}
	return out
}

// isOAuth says a server's Auth is an OAuth login state: every other value
// is a fixed word (authState).
func isOAuth(auth string) bool {
	switch auth {
	case "", "none", "static bearer", "static header":
		return false
	}
	return true
}

// infoState is what a server is doing, in the splash's words: running,
// starting (for how long), did not start (and why), or off. A scripted
// server runs when the shell does.
func infoState(st dun.ServerState, shellUp bool) string {
	switch {
	case st.Starting:
		if st.StartingSeconds > 0 {
			return fact(fmt.Sprintf("starting %ds", int(st.StartingSeconds)))
		}
		return "starting…"
	case st.Err != "":
		return fact("did not start:") + " " + oneLine(st.Err)
	case st.Via == dun.ViaShell && st.Auto && !shellUp:
		return "waits for the shell"
	case st.Via == dun.ViaShell && st.Auto, st.Via != dun.ViaShell && st.Running:
		return "running"
	}
	return "off"
}

// fact keeps a fact's words on one row: the conversation wraps at a space,
// and a no-break space is not one ("~1.1k" over "tokens" read as two facts).
func fact(s string) string { return strings.ReplaceAll(s, " ", "\u00a0") }

// infoMode is a server's mode: standalone, scripted, or built-in (which has
// no choice).
func infoMode(via string) string {
	if via == "" {
		return "built-in"
	}
	return dun.ModeWord(via)
}

// mcpHelp is /mcp help: the actions, one line each, from the command's own
// spec — the palette's and /help mcp's words.
func mcpHelp() string {
	for _, c := range ownCommands(false) {
		if c.Name != "mcp" {
			continue
		}
		w := 0
		for _, ch := range c.Choices {
			w = max(w, len(ch.Value))
		}
		rows := []string{"/mcp alone: a table of every server, on or off, and where that is set"}
		for _, ch := range c.Choices {
			rows = append(rows, "  "+pad(ch.Value, w)+"  "+ch.Description)
		}
		return strings.Join(rows, "\n")
	}
	return ""
}

// serverActions are the actions /mcp passes to its servers (servers.go,
// runServerCmd); the others it handles itself.
var serverActions = map[string]bool{"on": true, "off": true, "unset": true, "restart": true, "auto": true, "manual": true}

// unknownMCPAction is the one answer to /mcp <a> when a is no action: the
// nearest action when one is a typo away, and where the list is.
func unknownMCPAction(a string) string {
	best, bestD := "", 3 // a typo: at most two edits
	for _, c := range ownCommands(false) {
		if c.Name != "mcp" {
			continue
		}
		for _, ch := range c.Choices {
			if d := editDistance(a, ch.Value); d < bestD {
				best, bestD = ch.Value, d
			}
		}
	}
	msg := "unknown /mcp action " + strconv.Quote(a)
	if best != "" {
		msg += " — did you mean /mcp " + best + "?"
	}
	return msg + " /mcp help lists them"
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// costRows say what each server's schemas cost over the recorded rounds, in
// tokens, time and money, and what scripting saved (USER 2026-10-24: an
// excessive server is an anti-pattern; scripting keeps its tools without
// paying for the ones a turn does not need). Only the rounds that processed
// the prefix count: a cached prefix is nearly free to reuse, so counting
// every request would overstate it. The worst case, every request
// uncached, is the last row, at the measured speed.
func costRows(in mcpInfoIn, cost map[string]int) []string {
	u := in.usage
	var rows []string
	price := 0.0
	if u.CostTokens > 0 {
		price = u.Cost / float64(u.CostTokens)
	}
	spent := func(tok int) string {
		var out []string
		if in.speed > 0 {
			out = append(out, fact("~"+secs(float64(tok)/in.speed*1000)))
		}
		if price > 0 {
			out = append(out, fact("~"+turnMoney(float64(tok)*price, u.Currency)))
		}
		if len(out) == 0 {
			return ""
		}
		return " · " + strings.Join(out, " · ")
	}
	for _, st := range in.servers {
		su := u.Servers[st.ID]
		switch {
		case st.Via == dun.ViaShell && st.Standalone > 0:
			row := "  " + st.ID + ": scripted, saves " + fact("~"+tokN(st.Standalone)+" tokens") + " in every request"
			if su != nil && su.SavedRounds > 0 {
				row += "; " + fact(plural(su.SavedRounds, "request")) + " would have processed them: " + fact("~"+tokN(su.Saved)+" tokens") + spent(su.Saved)
			}
			if st.Help > 0 {
				row += " · help(\"" + dun.ShellNamespace(st.ID) + "\") " + fact("~"+tokN(st.Help)) + " once, when the model asks"
			}
			rows = append(rows, row)
		case st.Via != dun.ViaShell && cost[st.ID] > 0 && su != nil && su.Processed > 0:
			rows = append(rows, "  "+st.ID+": its "+fact("~"+tokN(cost[st.ID]))+" of schemas, processed in "+fact(plural(su.Processed, "request"))+": "+
				fact("~"+tokN(su.Tokens)+" tokens")+spent(su.Tokens))
		}
	}
	if len(rows) > 0 {
		head := fmt.Sprintf("what the schemas cost, last %d days (%s, %s processed the prefix; the rest reused it from the cache):",
			in.usageDays, plural(u.Rounds, "request"), fact(fmt.Sprintf("%d", u.Processed)))
		rows = append([]string{head}, rows...)
	}
	total := 0
	for _, n := range cost {
		total += n
	}
	switch {
	case in.speed > 0 && total > 0:
		rows = append(rows, "an uncached request processes all "+fact("~"+tokN(total)+" tokens")+" of tool schemas: "+
			fact("~"+secs(float64(total)/in.speed*1000))+" at "+fact(rate(in.speed))+" (measured over "+plural(in.speedRounds, "request")+")")
	case in.speed == 0 && u.Rounds > 0:
		rows = append(rows, "prefill speed: not measured — the endpoint reports no timings (llama.cpp does)")
	}
	return rows
}
