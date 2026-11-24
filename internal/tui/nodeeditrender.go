package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/iodesystems/dun/internal/textfmt"
)

type nodeEditDiag struct {
	File      string `json:"file"`
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	StartLine int    `json:"startLine"`
}

type nodeEditResult struct {
	Note              string         `json:"note"`
	Node              string         `json:"node"`
	BytesAdded        int            `json:"bytesAdded"`
	BytesRemoved      int            `json:"bytesRemoved"`
	Diagnostics       []nodeEditDiag `json:"diagnostics"`
	Validated         *bool          `json:"validated"`
	ValidationSkipped bool           `json:"validationSkipped"`
	Incomplete        bool           `json:"incomplete"`
	Action            string         `json:"action"`
	NewErrors         []string       `json:"newErrors"`
	Tip               string         `json:"tip"`
	Diff              *string        `json:"diff"`
}

func (d nodeEditDiag) line() string {
	return fmt.Sprintf("%s:%d: %s: %s", d.File, d.StartLine, d.Code, d.Message)
}

// warned: the edit landed but is not clean (the call mark shows ⚠). A skipped
// validation is not a warning.
func (v nodeEditResult) warned() bool {
	return v.Incomplete || (v.Validated != nil && !*v.Validated && !v.ValidationSkipped)
}

// parseNodeEdit reads a node_edit JSON result. ok is false for a non-JSON
// result or one that carries a diff, which keep the diff rendering.
func parseNodeEdit(result string) (v nodeEditResult, ok bool) {
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &v) != nil || v.Diff != nil {
		return nodeEditResult{}, false
	}
	return v, true
}

// nodeEditWarned is the narrow check behind the call mark: a parse, not a
// heuristic.
func nodeEditWarned(result string) bool {
	v, ok := parseNodeEdit(result)
	return ok && v.warned()
}

// nodeEditRender: node_edit answers JSON, not a diff. The preview carries the
// note, the byte delta, the diagnostic count and "incomplete" when the edit
// landed not clean; the body lists what is wrong, in the attention colour as
// evalRender's warnings are. A result with a diff field, or one that is not
// JSON, keeps the diff rendering.
func nodeEditRender(rc renderCtx) (string, string) {
	if rc.failed {
		return nodeEditFailed(rc)
	}
	v, ok := parseNodeEdit(rc.result)
	if !ok {
		return diffRender(rc)
	}
	head := v.Note
	if head == "" {
		head = v.Node
	}
	parts := []string{
		stSecondary.Render(textfmt.Clip(textfmt.OneLine(head), 200)),
		stSecondary.Render(fmt.Sprintf("+%d −%d", v.BytesAdded, v.BytesRemoved)),
	}
	if n := len(v.Diagnostics); n > 0 {
		parts = append(parts, stSecondary.Render(textfmt.Plural(n, "diagnostic")))
	}
	if v.warned() {
		parts = append(parts, stNote.Render("incomplete"))
	}
	pv := StDim.Render("  → ") + strings.Join(parts, stSecondary.Render(" · "))

	var warns []string
	if v.Action != "" {
		warns = append(warns, v.Action)
	}
	warns = append(warns, v.NewErrors...)
	for _, d := range v.Diagnostics {
		warns = append(warns, d.line())
	}
	if v.Tip != "" {
		warns = append(warns, "tip: "+v.Tip)
	}
	if len(warns) == 0 {
		return pv, stSecondary.Render(head)
	}
	return pv, withWarnings("", warns)
}

// nodeEditFailed keeps the failure preview and body; when the error carries the
// JSON array ([{"action":…,"diagnostics":[…]}]) it renders those lines too.
func nodeEditFailed(rc renderCtx) (string, string) {
	pv, body := diffRender(rc)
	i := strings.Index(rc.result, "[")
	if i < 0 {
		return pv, body
	}
	var arr []struct {
		Action      string         `json:"action"`
		Diagnostics []nodeEditDiag `json:"diagnostics"`
	}
	if json.NewDecoder(strings.NewReader(rc.result[i:])).Decode(&arr) != nil {
		return pv, body
	}
	lines := []string{StErr.Render(textfmt.Clip(textfmt.OneLine(strings.TrimSpace(rc.result[:i])), previewClip))}
	for _, e := range arr {
		if e.Action != "" {
			lines = append(lines, stNote.Render(e.Action))
		}
		for _, d := range e.Diagnostics {
			lines = append(lines, stNote.Render(d.line()))
		}
	}
	if len(lines) == 1 {
		return pv, body
	}
	return pv, strings.Join(lines, "\n")
}
