package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tool-result renderers. A renderer turns one tool's result into a collapsed
// one-line preview + a full expanded body (the TUI folds the call line + ▸/▾
// around them). Keyed by tool name; unknown tools fall to genericRender.
//
// Loading is COMPILED-IN for now (registerRenderer in init). The ToolRenderer
// interface is the seam: a later runtime mechanism — an external `dun-render-
// <tool>` process, or a script/config-backed renderer — implements the same
// interface and registers itself, with no change to the call sites here or in
// the event handler.

type renderCtx struct {
	tool   string
	args   map[string]any
	result string
	width  int
}

// ToolRenderer turns a tool result into (collapsed preview, full body).
type ToolRenderer interface {
	Render(rc renderCtx) (preview, full string)
}

type rendererFunc func(renderCtx) (string, string)

func (f rendererFunc) Render(rc renderCtx) (string, string) { return f(rc) }

var toolRenderers = map[string]ToolRenderer{}

func registerRenderer(tool string, r ToolRenderer) { toolRenderers[tool] = r }

// renderToolResult dispatches to a registered renderer or the generic fallback.
func renderToolResult(rc renderCtx) (preview, full string) {
	if r, ok := toolRenderers[rc.tool]; ok {
		return r.Render(rc)
	}
	return genericRender(rc)
}

// genericRender: a clipped one-line preview + the raw body, diff-colorized when
// it looks like a unified diff (the prior default behavior, now the fallback).
func genericRender(rc renderCtx) (string, string) {
	preview := stDim.Render("  → ") + stSecondary.Render(clip(oneLine(rc.result), 100))
	if isDiff(rc.result) {
		return preview, colorizeDiff(rc.result)
	}
	return preview, stSecondary.Render(rc.result)
}

// diffRender always colorizes; the preview is an add/del line stat.
func diffRender(rc renderCtx) (string, string) {
	return stDim.Render("  → ") + stSecondary.Render(diffStat(rc.result)), colorizeDiff(rc.result)
}

// jsonRender pretty-prints a JSON result; the preview summarizes shape (item
// count / top-level keys). Non-JSON falls back to generic.
func jsonRender(rc renderCtx) (string, string) {
	var v any
	if json.Unmarshal([]byte(strings.TrimSpace(rc.result)), &v) != nil {
		return genericRender(rc)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return genericRender(rc)
	}
	return stDim.Render("  → ") + stSecondary.Render(jsonSummary(v)), stSecondary.Render(string(pretty))
}

func jsonSummary(v any) string {
	switch t := v.(type) {
	case []any:
		return fmt.Sprintf("%d item(s)", len(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return clip(strings.Join(keys, ", "), 100)
	default:
		return clip(oneLine(fmt.Sprint(v)), 100)
	}
}

// diffStat counts +/- body lines of a unified diff, e.g. "+3 -1".
func diffStat(s string) string {
	add, del := 0, 0
	for _, ln := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++"):
			add++
		case strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---"):
			del++
		}
	}
	if add == 0 && del == 0 {
		return clip(oneLine(s), 100)
	}
	return fmt.Sprintf("+%d -%d", add, del)
}

func init() {
	// Code edits return unified diffs (+ diagnostics) — always colorize.
	registerRenderer("node_edit", rendererFunc(diffRender))
	// These commonly return JSON payloads; pretty-print, else generic.
	registerRenderer("search", rendererFunc(jsonRender))
	registerRenderer("node_query", rendererFunc(jsonRender))
	registerRenderer("list_documents", rendererFunc(jsonRender))
	registerRenderer("list_indexes", rendererFunc(jsonRender))
	registerRenderer("index_status", rendererFunc(jsonRender))
	registerRenderer("search_figures", rendererFunc(jsonRender))
	// node_read returns source code — show what it read.
	registerRenderer("node_read", rendererFunc(nodeReadRender))
	// eval is mcpshell — its own renderer (evalrender.go).
	registerRenderer("eval", rendererFunc(evalRender))
	// get_document returns indexed text — show path + pages.
	registerRenderer("get_document", rendererFunc(getDocRender))
	// ocr extracts pages — show page count.
	registerRenderer("ocr", rendererFunc(ocrRender))
	// ingest queues a job — show the job id.
	registerRenderer("ingest", rendererFunc(ingestRender))
	// recap summarizes context — show what was replaced.
	registerRenderer("recap", rendererFunc(recapRender))

	// dun's own tools. Each of these was a clipped sentence that hid its verdict.
	registerRenderer("exec", rendererFunc(execRender))
	registerRenderer("jobs", rendererFunc(jobsRender))
	registerRenderer("agent", rendererFunc(agentRender))
	registerRenderer("session", rendererFunc(sessionRender))
	// The tools jobs replaced, for transcripts written before it.
	registerRenderer("exec_monitor", rendererFunc(execMonitorRender))
	registerRenderer("agent_monitor", rendererFunc(agentMonitorRender))
	registerRenderer("tell_parent", rendererFunc(tellParentRender))
	registerRenderer("dun_msg", rendererFunc(dunMsgRender))
	registerRenderer("ask_parent", rendererFunc(askRender))
	registerRenderer("ask_user", rendererFunc(askRender))
}

// ── dun's own tools ─────────────────────────────────────────────────
//
// Everything below renders a tool dun itself owns, where the collapsed line was
// a clipped sentence that hid the one thing worth knowing: did it work. The
// pattern is the same each time — the preview carries the VERDICT and the body
// stays verbatim, because a preview that summarizes away a failure is worse
// than no preview.

// execRender: did the command work, and if not, why.
func execRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	if isDiff(rc.result) {
		body = colorizeDiff(rc.result)
	}
	// A background start is not a result — it is a receipt. Say so, and carry
	// the job id, because that id is the only handle on it afterwards.
	if job, ok := backgroundJobID(rc.result); ok {
		return stDim.Render("  → ") + stSecondary.Render("⏱ background job #"+job) +
			stSecondary.Render(" · jobs(job:"+job+") to check on it"), body
	}

	marker, failed := execExitMarker(rc.result)
	switch {
	case failed && strings.Contains(marker, "TIMED OUT"):
		return stDim.Render("  → ") + stErr.Render("⏱ TIMED OUT") +
			stSecondary.Render(" · "+clip(oneLine(execTail(rc.result)), 70)), body
	case failed:
		return stDim.Render("  → ") + stErr.Render("✗ "+clip(marker, 30)) +
			stSecondary.Render(" · "+clip(oneLine(execTail(rc.result)), 70)), body
	}
	n := countLines(rc.result)
	head := stDim.Render("  → ") + stTool.Render("✓")
	if n == 0 {
		return head + stSecondary.Render(" (no output)"), body
	}
	return head + stSecondary.Render(fmt.Sprintf(" %s · %s", plural(n, "line"),
		clip(oneLine(firstLine(rc.result)), 70))), body
}

// execExitMarker reads the "[exit: …]" tail ExecResult.Render appends.
//
// It looks at the LAST line only, deliberately. `strings.Contains` is what the
// exec code itself was just rewritten to stop doing — a command that PRINTS
// that marker is not a failing command — and although this is presentation and
// not control flow, a preview that says ✗ about a passing command is still a
// lie. The marker is always appended last, so position is what identifies it.
func execExitMarker(result string) (marker string, failed bool) {
	last := lastLine(result)
	if !strings.HasPrefix(last, "[exit:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(last, "[exit:"), "]")), true
}

// execTail is the last INFORMATIVE thing the command said before it failed.
//
// The error is at the bottom of a build log, not the top — but the very last
// line is usually a verdict, not the reason: `go test` ends with a bare "FAIL",
// make with "make: *** [all] Error 1". So it walks backwards to the first line
// that actually carries something, which in practice is the one naming a file
// and a line. A preview that says "FAIL" has spent its space saying nothing.
func execTail(result string) string {
	lines := meaningfulLines(result)
	// Drop the marker itself; the caller already has it.
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "[exit:") {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return "(no output)"
	}
	for i := len(lines) - 1; i >= 0 && i > len(lines)-6; i-- {
		if informative(lines[i]) {
			return lines[i]
		}
	}
	return lines[len(lines)-1]
}

// informative rejects the bare verdicts a failing build ends with. Bounded and
// dumb on purpose: anything cleverer becomes a per-toolchain parser, and the
// full output is one keypress away.
func informative(line string) bool {
	switch strings.ToUpper(strings.TrimRight(line, ".!")) {
	case "FAIL", "FAILED", "ERROR", "ERRORS", "OK", "PASS":
		return false
	}
	// The shell's own epilogue, which restates the exit code the preview is
	// already showing.
	if strings.HasPrefix(line, "exit status ") {
		return false
	}
	// A colon is the near-universal shape of "where: what". Failing that, only
	// a line long enough to be a sentence is worth the preview's space.
	return strings.Contains(line, ":") || len(line) > 20
}

// backgroundJobID pulls the id out of exec's background receipt: "It is now
// background job #N" (bgJob.handoffNotice), or the older "Started background
// job #N".
func backgroundJobID(result string) (string, bool) {
	rest := ""
	for _, prefix := range []string{"is now background job #", "Started background job #"} {
		if i := strings.Index(result, prefix); i >= 0 {
			rest = result[i+len(prefix):]
			break
		}
	}
	if rest == "" {
		return "", false
	}
	end := strings.IndexAny(rest, ":. \n")
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}

// execMonitorRender: the job listing, or one job's state.
func execMonitorRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	switch {
	case strings.HasPrefix(first, "No background jobs"):
		return stDim.Render("  → ") + stSecondary.Render("no background jobs"), body
	case strings.HasPrefix(first, "ERROR"):
		return stDim.Render("  → ") + stErr.Render(clip(first, 90)), body
	}
	// A listing is one line per job; a single-job report starts with its state.
	if n := countMatching(rc.result, "#"); n > 1 && !strings.Contains(first, "monitor:") {
		return stDim.Render("  → ") + stSecondary.Render(plural(n, "job")), body
	}
	return stDim.Render("  → ") + stSecondary.Render(clip(oneLine(first), 90)), body
}

// jobsRender: the jobs tool answers for a command's job (job:N), a
// sub-agent (agent:N), or lists both kinds ("job #…" and "agent #…" lines).
func jobsRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	switch {
	case strings.HasPrefix(first, "No background jobs"):
		return stDim.Render("  → ") + stSecondary.Render("no background jobs"), body
	case strings.HasPrefix(first, "ERROR"):
		return stDim.Render("  → ") + stErr.Render(clip(first, 90)), body
	}
	jobs, agents, other := 0, 0, false
	for _, ln := range strings.Split(strings.TrimSpace(rc.result), "\n") {
		switch {
		case strings.HasPrefix(ln, "job #"):
			jobs++
		case strings.HasPrefix(ln, "agent #"):
			agents++
		default:
			other = true
		}
	}
	if !other && jobs+agents > 0 {
		var parts []string
		if jobs > 0 {
			parts = append(parts, plural(jobs, "job"))
		}
		if agents > 0 {
			parts = append(parts, plural(agents, "agent"))
		}
		return stDim.Render("  → ") + stSecondary.Render(strings.Join(parts, " · ")), body
	}
	if _, ok := rc.args["agent"]; ok || strings.HasPrefix(first, "agent ") {
		return agentMonitorRender(rc)
	}
	return execMonitorRender(rc)
}

// sessionRender: the session tool returns {harness, state} as JSON. A call
// that set the state shows the state; a read shows what is running.
func sessionRender(rc renderCtx) (string, string) {
	var v struct {
		Harness struct {
			Version string `json:"version"`
			Model   string `json:"model"`
		} `json:"harness"`
		State struct {
			Goal   string `json:"goal"`
			Status string `json:"status"`
		} `json:"state"`
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(rc.result)), &v) != nil {
		return genericRender(rc)
	}
	body := stSecondary.Render(rc.result)
	if v.Error != "" {
		return stDim.Render("  → ") + stErr.Render(clip(v.Error, 90)), body
	}
	if len(rc.args) > 0 {
		var parts []string
		if v.State.Status != "" {
			parts = append(parts, v.State.Status)
		}
		if v.State.Goal != "" {
			parts = append(parts, v.State.Goal)
		}
		return stDim.Render("  → ") + stSecondary.Render("state: "+clip(oneLine(strings.Join(parts, " · ")), 90)), body
	}
	return stDim.Render("  → ") + stSecondary.Render(clip("dun "+v.Harness.Version+" · "+v.Harness.Model, 90)), body
}

// agentRender: which child, what state, and what it cost — the spend is the
// argument for delegating at all, and it is invisible in a clipped line.
func agentRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	switch {
	case strings.HasPrefix(first, "ERROR"):
		return stDim.Render("  → ") + stErr.Render(clip(first, 90)), body
	case strings.HasPrefix(first, "Started agent"):
		return stDim.Render("  → ") + stSecondary.Render("▸ "+clip(strings.TrimSuffix(first, "."), 80)), body
	}
	if id, state, cost, ok := agentReportHead(first); ok {
		// Running and idle are the normal course; a failure is red. Orange is
		// for an OLD transcript's blocked child (ask_parent used to block;
		// a child now asks by ending its turn, so no live report says this).
		style := stSecondary
		switch strings.ToUpper(state) {
		case "IDLE", "RUNNING":
			style = stTool
		case "FAILED", "DISMISSED":
			style = stErr
		}
		if strings.Contains(rc.result, "\nBLOCKED, waiting on you") {
			state, style = "blocked", stNote
		}
		out := stDim.Render("  → ") + style.Render(fmt.Sprintf("#%s %s", id, strings.ToLower(state)))
		if cost != "" {
			out += stSecondary.Render(" · " + cost)
		}
		return out, body
	}
	return genericRender(rc)
}

// agentReportHead parses the report's first line, "agent #2 is idle after
// 3m40s (41.2k tokens) on m" (subAgent.report), and the older "agent #2 IDLE
// after 3m40s — spent 41.2k tokens…".
func agentReportHead(first string) (id, state, cost string, ok bool) {
	f := strings.Fields(first)
	if len(f) < 3 || f[0] != "agent" || !strings.HasPrefix(f[1], "#") {
		return "", "", "", false
	}
	id, state = strings.TrimPrefix(f[1], "#"), f[2]
	if state == "is" && len(f) > 3 {
		state = f[3]
	}
	if i := strings.Index(first, "spent "); i >= 0 {
		cost = clip(oneLine(first[i:]), 40)
	} else if i, j := strings.Index(first, " ("), strings.Index(first, " tokens)"); i >= 0 && j > i {
		cost = first[i+2 : j+len(" tokens")]
	}
	return id, state, cost, true
}

// agentMonitorRender: a listing, or one child's report.
func agentMonitorRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	switch {
	case strings.HasPrefix(first, "No sub-agents"):
		return stDim.Render("  → ") + stSecondary.Render("no sub-agents"), body
	case strings.HasPrefix(first, "ERROR"), strings.HasPrefix(first, "Could not"):
		return stDim.Render("  → ") + stErr.Render(clip(first, 90)), body
	case strings.HasPrefix(first, "Answered"), strings.HasPrefix(first, "Told"),
		strings.HasPrefix(first, "Dismissed"), strings.HasPrefix(first, "Restarted"):
		return stDim.Render("  → ") + stSecondary.Render(clip(oneLine(first), 90)), body
	}
	if strings.HasPrefix(first, "agent ") {
		return agentRender(rc)
	}
	if n := countMatching(rc.result, "#"); n > 0 {
		return stDim.Render("  → ") + stSecondary.Render(plural(n, "agent")), body
	}
	return genericRender(rc)
}

// tellParentRender reads the ARGS, not the result. "status set." tells you
// nothing; what the child actually said is the point.
func tellParentRender(rc renderCtx) (string, string) {
	var parts []string
	if s := argStr(rc.args, "final"); s != "" {
		parts = append(parts, stTool.Render("✓ final: ")+stSecondary.Render(clip(oneLine(s), 80)))
	}
	if s := argStr(rc.args, "message"); s != "" {
		parts = append(parts, stSecondary.Render("✉ ")+stSecondary.Render(clip(oneLine(s), 80)))
	}
	if s := argStr(rc.args, "status"); s != "" && len(parts) == 0 {
		parts = append(parts, stSecondary.Render("· "+clip(oneLine(s), 80)))
	}
	if len(parts) == 0 {
		return genericRender(rc)
	}
	return stDim.Render("  → ") + strings.Join(parts, stSecondary.Render(" · ")), stSecondary.Render(rc.result)
}

// dunMsgRender reads the ARGS for who and what, like tellParentRender; the
// result only says whether it arrived, so a refusal is shown in red.
func dunMsgRender(rc renderCtx) (string, string) {
	to, text := argStr(rc.args, "to"), argStr(rc.args, "text")
	if to == "" {
		return genericRender(rc)
	}
	head := stSecondary.Render("✉ "+to+": ") + stSecondary.Render(clip(oneLine(text), 70))
	if strings.HasPrefix(rc.result, "ERROR") {
		head += stErr.Render("  " + clip(oneLine(strings.TrimPrefix(rc.result, "ERROR: ")), 60))
	}
	return stDim.Render("  → ") + head, stSecondary.Render(text + "\n\n" + rc.result)
}

// askRender is for ask_user and ask_parent (a hidden alias now; old
// transcripts still show it): the question is in the args and the ANSWER is
// the result, so show the answer and keep both in the body.
func askRender(rc renderCtx) (string, string) {
	q := argStr(rc.args, "question")
	ans := oneLine(rc.result)
	style := stAsk
	if strings.HasPrefix(rc.result, "ERROR") {
		style = stErr
	}
	body := stSecondary.Render(rc.result)
	if q != "" {
		body = stAsk.Render("❓ "+q) + "\n" + stSecondary.Render(rc.result)
	}
	return stDim.Render("  → ") + style.Render("❝"+clip(ans, 88)+"❞"), body
}

func argStr(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// nodeReadRender: what was read and how much.
func nodeReadRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	n := countLines(rc.result)
	head := stDim.Render("  → ") + stTool.Render("✓")
	if n == 0 {
		return head + stSecondary.Render(" (empty)"), body
	}
	return head + stSecondary.Render(fmt.Sprintf(" %s · %s", plural(n, "line"),
		clip(oneLine(firstLine(rc.result)), 70))), body
}

// getDocRender: which document, how many pages.
func getDocRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	if strings.HasPrefix(first, "error") || strings.HasPrefix(first, "Error") {
		return stDim.Render("  → ") + stErr.Render("✗ "+clip(first, 90)), body
	}
	n := countLines(rc.result)
	return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(fmt.Sprintf(" %s · %s",
		plural(n, "line"), clip(first, 70))), body
}

// ocrRender: how many pages were extracted.
func ocrRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	if strings.HasPrefix(first, "error") || strings.HasPrefix(first, "Error") {
		return stDim.Render("  → ") + stErr.Render("✗ "+clip(first, 90)), body
	}
	// OCR output mentions page counts; fall back to line count.
	pages := countMatching(rc.result, "page")
	if pages > 0 {
		return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(" "+plural(pages, "page")), body
	}
	n := countLines(rc.result)
	return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(" "+plural(n, "line")), body
}

// ingestRender: the job id or status.
func ingestRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	if strings.HasPrefix(first, "error") || strings.HasPrefix(first, "Error") {
		return stDim.Render("  → ") + stErr.Render("✗ "+clip(first, 90)), body
	}
	return stDim.Render("  → ") + stSecondary.Render("⏱ queued") + stSecondary.Render(" · "+clip(first, 80)), body
}

// recapRender: what was replaced and how much was saved.
func recapRender(rc renderCtx) (string, string) {
	body := stSecondary.Render(rc.result)
	first := firstLine(rc.result)
	if strings.HasPrefix(first, "error") || strings.HasPrefix(first, "Error") {
		return stDim.Render("  → ") + stErr.Render("✗ "+clip(first, 90)), body
	}
	return stDim.Render("  → ") + stSecondary.Render("↺ recap") + stSecondary.Render(" · "+clip(first, 80)), body
}

func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := meaningfulLines(s)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func meaningfulLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func countLines(s string) int { return len(meaningfulLines(s)) }

func countMatching(s, prefix string) int {
	n := 0
	for _, ln := range meaningfulLines(s) {
		if strings.HasPrefix(ln, prefix) {
			n++
		}
	}
	return n
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
