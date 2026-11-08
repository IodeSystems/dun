package dun

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
)

// Recap stubs: the level between keep and drop.
//
// A recap's summary loses which commands ran and how each ended. In long
// Terminal-Bench trials cheap models then ran again attempts they no longer
// remembered. So each removed call that is not kept or dropped stays in
// context as one line: the tool, a short form of its arguments, the outcome,
// the result size, and the path of a file that holds the result. file_read on
// that path is the one way back; the recapN.jsonl sidecar is the audit trail,
// not a read surface.

// recapStubMax is how many removed calls get a line each. The earlier ones in
// the span become one count line: a stub costs some 30-45 tokens, and a span
// of 200 polls must not cost 8k tokens to list.
const recapStubMax = 40

// recapArgsMax is the length of a stub's argument form, in runes.
const recapArgsMax = 80

// recapStubHead opens the stub list in the replacement. Also how a later recap
// in the same turn finds the lines of an earlier one and carries them on.
const recapStubHead = "Calls removed from context (outcome, result size, file with the result; read it with file_read):"

// removedCall is one tool call a recap takes out, with its result.
type removedCall struct {
	Call, Result agent.Entry
}

// stubLine is one line of the list: the text, and what the count line needs.
type stubLine struct {
	tool, outcome, path, text string
}

// noteExecVerdict files how an exec call ended, by tool call id.
func (h *Harness) noteExecVerdict(callID, verdict string) {
	if callID == "" {
		return
	}
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	if h.execVerdicts == nil {
		h.execVerdicts = map[string]string{}
	}
	h.execVerdicts[callID] = verdict
}

func (h *Harness) execVerdictOf(callID string) string {
	h.recapMu.Lock()
	defer h.recapMu.Unlock()
	return h.execVerdicts[callID]
}

// execVerdict is a stub's outcome for an exec: from the ExecResult, not its text.
func execVerdict(r ExecResult, done bool, job int) string {
	switch {
	case !done:
		return fmt.Sprintf("still running as job #%d", job)
	case r.Killed != "":
		return "killed"
	case r.Err != "":
		return "ERROR"
	case r.Code != 0:
		return fmt.Sprintf("exit %d", r.Code)
	}
	return "ok"
}

var exitStatusLine = regexp.MustCompile(`\[exit: status (-?[0-9]+)\]$`)

// callOutcome is ok, exit N, killed, ERROR or still running. An exec uses
// the recorded verdict. Without one (a resumed session: the record is in
// memory only) it reads the result's last line, which renderForeground keeps
// as the exit line.
func (h *Harness) callOutcome(rc removedCall) string {
	res := rc.Result.Content
	if rc.Call.ToolName != "exec" {
		if strings.HasPrefix(res, "ERROR") {
			return "ERROR"
		}
		return "ok"
	}
	if v := h.execVerdictOf(rc.Call.ToolCallID); v != "" {
		return v
	}
	if items := liftedItems(res); len(items) > 0 {
		if i := strings.Index(res, items[0]); i >= 0 {
			res = res[:i]
		}
	}
	res = strings.TrimSpace(res)
	last := res[strings.LastIndexByte(res, '\n')+1:]
	switch {
	case exitStatusLine.MatchString(last):
		return "exit " + exitStatusLine.FindStringSubmatch(last)[1]
	case strings.HasPrefix(last, "[exit: killed"):
		return "killed"
	case strings.HasPrefix(last, "[exit: "), strings.HasPrefix(res, "ERROR"):
		return "ERROR"
	case strings.Contains(res, "is still running after") && strings.Contains(res, "background job #"):
		return "still running"
	}
	return "ok"
}

// shortArgs is a call's arguments in at most recapArgsMax runes: an exec's
// command (first line), else the JSON on one line.
func shortArgs(call agent.Entry) string {
	s := call.Content
	if call.ToolName == "exec" {
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Content), &a) == nil && a.Command != "" {
			cmd := strings.TrimSpace(a.Command)
			first, rest, more := strings.Cut(cmd, "\n")
			s = strings.TrimSpace(first)
			if more && strings.TrimSpace(rest) != "" {
				s += " …"
			}
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > recapArgsMax {
		s = string(r[:recapArgsMax-1]) + "…"
	}
	return s
}

// recapFileTag names a recap's result files: recapN after the sidecar, so a
// later recap in the session, or one after a resume, cannot overwrite them.
func recapFileTag(sidecar string) string {
	if sidecar == "" {
		return "recap-" + uuid.New().String()[:8]
	}
	return strings.TrimPrefix(filepath.Ext(strings.TrimSuffix(sidecar, ".jsonl")), ".")
}

// saveRemoved writes each removed call's result to a file of its own and
// returns the stub lines, oldest first.
func (h *Harness) saveRemoved(calls []removedCall, tag string) []stubLine {
	var out []stubLine
	for k, rc := range calls {
		head := rc.Call.ToolName + " " + rc.Call.Content
		if rc.Call.ToolName == "exec" {
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(rc.Call.Content), &a) == nil && a.Command != "" {
				head = "$ " + a.Command
			}
		}
		body := head + "\n\n" + rc.Result.Content
		path := h.saveBoth(fmt.Sprintf("%s-%d.out", tag, k+1), []byte(body))
		where := path
		if where == "" {
			where = "(not saved)"
		}
		oc := h.callOutcome(rc)
		out = append(out, stubLine{
			tool: rc.Call.ToolName, outcome: oc, path: path,
			text: fmt.Sprintf("- %s: %s → %s, %d chars, %s",
				rc.Call.ToolName, shortArgs(rc.Call), oc, len(rc.Result.Content), where),
		})
	}
	return out
}

// carriedStubs are the stub lines of an earlier recap in this turn whose
// replacement this recap removes: those calls stay listed. A count line is
// carried as it is (counts = its text), after nothing else.
func carriedStubs(subsumed []agent.Entry) (lines []stubLine, counts []string) {
	for _, e := range subsumed {
		if e.Kind != agent.KindAssistant {
			continue
		}
		_, block, ok := strings.Cut(e.Content, recapStubHead+"\n")
		if !ok {
			continue
		}
		for _, l := range strings.Split(block, "\n") {
			if !strings.HasPrefix(l, "- ") {
				if strings.TrimSpace(l) != "" {
					counts = append(counts, l)
				}
				continue
			}
			tool, rest, _ := strings.Cut(l[2:], ": ")
			sl := stubLine{tool: tool, text: l}
			if i := strings.LastIndex(rest, " → "); i >= 0 {
				oc, tail, _ := strings.Cut(rest[i+len(" → "):], ", ")
				sl.outcome = oc
				if j := strings.LastIndex(tail, ", "); j >= 0 {
					sl.path = tail[j+2:]
				}
			}
			lines = append(lines, sl)
		}
	}
	return lines, counts
}

// stubBlock is the list as it goes into the replacement: at most recapStubMax
// lines of their own, the earlier ones as one count line. "" for no calls.
func stubBlock(lines []stubLine, counts []string) string {
	if len(lines) == 0 && len(counts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(recapStubHead)
	for _, c := range counts {
		b.WriteString("\n" + c)
	}
	if n := len(lines) - recapStubMax; n > 0 {
		b.WriteString("\n" + countLine(lines[:n]))
		lines = lines[n:]
	}
	for _, l := range lines {
		b.WriteString("\n" + l.text)
	}
	return b.String()
}

// countLine is "M earlier calls: K exec (J failed), … — results in <dir>".
func countLine(lines []stubLine) string {
	total, failed := map[string]int{}, map[string]int{}
	dir := ""
	for _, l := range lines {
		total[l.tool]++
		if l.outcome != "ok" && !strings.HasPrefix(l.outcome, "still running") {
			failed[l.tool]++
		}
		if l.path != "" && l.path != "(not saved)" {
			dir = filepath.Dir(l.path)
		}
	}
	tools := make([]string, 0, len(total))
	for t := range total {
		tools = append(tools, t)
	}
	sort.Slice(tools, func(a, b int) bool {
		if total[tools[a]] != total[tools[b]] {
			return total[tools[a]] > total[tools[b]]
		}
		return tools[a] < tools[b]
	})
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		p := strconv.Itoa(total[t]) + " " + t
		if failed[t] > 0 {
			p += fmt.Sprintf(" (%d failed)", failed[t])
		}
		parts = append(parts, p)
	}
	where := "results not saved"
	if dir != "" {
		where = "results in " + dir + " (the first line of each file is the call)"
	}
	return fmt.Sprintf("%d earlier call%s: %s — %s", len(lines), plural(len(lines)), strings.Join(parts, ", "), where)
}
