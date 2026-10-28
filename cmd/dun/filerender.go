package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The built-in file tools (dun/files.go, agentkit/files), rendered so a step
// reads as what it did:
//
//	✓ read internal/a.go:10-40 (31 lines)
//	✓ edit internal/a.go +3 −1
//	✓ write docs/new.md (12 lines, new)
//	✓ grep /needle/ → 12 matches in 3 files
//	✓ list internal/ → 2 dirs, 9 files
//
// file_read also lists a directory and, with grep, searches (agentkit
// v0.14): a read with grep is drawn as a grep, a read of a directory as a
// list. file_grep and file_list keep their renderers, for configs that name
// them and for older transcripts.
//
// The call half comes from fileCallText (callText), the outcome half from
// the renderer's preview, parsed from the result's first line — the line
// agentkit/files writes for the model, so the two cannot drift apart
// without these tests failing.

const filePrefix = "file_"

func isFileTool(tool string) bool { return strings.HasPrefix(tool, filePrefix) }

// fileCallText is a file tool's call as a short verb phrase. ok=false: not
// a file tool, or no usable args.
func fileCallText(tool string, args map[string]any, max int) (string, bool) {
	if !isFileTool(tool) {
		return "", false
	}
	verb := strings.TrimPrefix(tool, filePrefix)
	path := argStr(args, "path")
	if verb == "read" && readGrep(args) != "" {
		verb = "grep"
		if path == "." {
			path = ""
		}
		args = map[string]any{"pattern": readGrep(args), "path": path, "glob": argStr(args, "glob")}
	}
	switch verb {
	case "read", "edit", "write", "delete":
		if path == "" {
			return "", false
		}
		return clip(verb+" "+path, max), true
	case "move":
		return clip("move "+path+" → "+argStr(args, "to"), max), true
	case "grep":
		pat := argStr(args, "pattern")
		if pat == "" {
			return "", false
		}
		s := "grep /" + oneLine(pat) + "/"
		if path != "" {
			s += " in " + path
		}
		if g := argStr(args, "glob"); g != "" {
			s += " " + g
		}
		return clip(s, max), true
	case "list":
		s := "list"
		if path != "" {
			s += " " + path
		}
		if g := argStr(args, "glob"); g != "" {
			s += " " + g
		}
		return clip(s, max), true
	}
	return "", false
}

// readGrep is a file_read call's search: grep, or grep's own name for it.
func readGrep(args map[string]any) string {
	if g := argStr(args, "grep"); g != "" {
		return g
	}
	return argStr(args, "pattern")
}

var (
	reReadHead  = regexp.MustCompile(`^.*: lines (\d+)-(\d+) of (\d+)$`)
	reReadWhole = regexp.MustCompile(`^.*: (\d+) lines?$`)
	reEditHead  = regexp.MustCompile(`^edited .*: \+(\d+) -(\d+)`)
	reWriteHead = regexp.MustCompile(`^wrote .* \((\d+ lines?), (new|overwritten)`)
	reGrepHead  = regexp.MustCompile(`^(\d+\+?) (match|matches) in (\d+\+?) files?`)
	reGrepFiles = regexp.MustCompile(`^(\d+\+?) files? match`)
)

// fileFailed is the preview of a refused call: its reason, clipped.
func fileFailed(result string) (string, string, bool) {
	r := strings.TrimSpace(result)
	if !strings.HasPrefix(r, "ERROR") {
		return "", "", false
	}
	why := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r, "ERROR:"), "ERROR"))
	return stDim.Render(" → ") + stErr.Render(clip(oneLine(firstLine(why)), 80)), stSecondary.Render(result), true
}

func fileReadRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	if readGrep(rc.args) != "" {
		return fileGrepRender(rc)
	}
	head := firstLine(rc.result)
	body := stSecondary.Render(rc.result)
	if m := reReadHead.FindStringSubmatch(head); m != nil {
		from, _ := strconv.Atoi(m[1])
		to, _ := strconv.Atoi(m[2])
		return stSecondary.Render(fmt.Sprintf(":%d-%d (%s of %s)", from, to, plural(to-from+1, "line"), m[3])), body
	}
	if m := reReadWhole.FindStringSubmatch(head); m != nil {
		n, _ := strconv.Atoi(m[1])
		return stSecondary.Render(fmt.Sprintf(" (%s)", plural(n, "line"))), body
	}
	return fileListRender(rc) // a directory; its fallback is the clipped head
}

func fileEditRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	if m := reEditHead.FindStringSubmatch(firstLine(rc.result)); m != nil {
		return " " + diffAdd.Render("+"+m[1]) + " " + diffDel.Render("−"+m[2]), colorizeDiff(rc.result)
	}
	return stDim.Render(" → ") + stSecondary.Render(clip(oneLine(firstLine(rc.result)), 80)), colorizeDiff(rc.result)
}

func fileWriteRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	if m := reWriteHead.FindStringSubmatch(firstLine(rc.result)); m != nil {
		return stSecondary.Render(" (" + m[1] + ", " + m[2] + ")"), stSecondary.Render(rc.result)
	}
	return stDim.Render(" → ") + stSecondary.Render(clip(oneLine(firstLine(rc.result)), 80)), stSecondary.Render(rc.result)
}

func fileGrepRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	body := stSecondary.Render(rc.result)
	lines := strings.SplitN(rc.result, "\n", 3)
	head := lines[0]
	if strings.HasPrefix(head, "searched for ") && len(lines) > 1 { // the literal-fallback note
		head = lines[1]
	}
	switch m := reGrepHead.FindStringSubmatch(head); {
	case m != nil:
		return stDim.Render(" → ") + stSecondary.Render(m[1]+" "+m[2]+" in "+m[3]+" "+pluralWord(m[3], "file")), body
	case strings.HasPrefix(head, "no matches"):
		return stDim.Render(" → ") + stSecondary.Render("no matches"), body
	}
	if m := reGrepFiles.FindStringSubmatch(head); m != nil {
		return stDim.Render(" → ") + stSecondary.Render(m[1]+" "+pluralWord(m[1], "file")), body
	}
	return stDim.Render(" → ") + stSecondary.Render(clip(oneLine(head), 80)), body
}

var reListDir = regexp.MustCompile(`^.*/: (\d+ dirs?), (\d+ files?)`)

func fileListRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	head := firstLine(rc.result)
	body := stSecondary.Render(rc.result)
	if m := reListDir.FindStringSubmatch(head); m != nil {
		return stDim.Render(" → ") + stSecondary.Render(m[1]+", "+m[2]), body
	}
	if m := reGrepFiles.FindStringSubmatch(head); m != nil {
		return stDim.Render(" → ") + stSecondary.Render(m[1]+" "+pluralWord(m[1], "file")), body
	}
	return stDim.Render(" → ") + stSecondary.Render(clip(oneLine(head), 80)), body
}

// fileOtherRender: delete and move say what they did in one line.
func fileOtherRender(rc renderCtx) (string, string) {
	if p, b, ok := fileFailed(rc.result); ok {
		return p, b
	}
	return stSecondary.Render(" ✓"), stSecondary.Render(rc.result)
}

// pluralWord is "file" for "1" and "files" for anything else ("2", "10+").
func pluralWord(n, word string) string {
	if n == "1" {
		return word
	}
	return word + "s"
}

func init() {
	registerRenderer(filePrefix+"read", rendererFunc(fileReadRender))
	registerRenderer(filePrefix+"edit", rendererFunc(fileEditRender))
	registerRenderer(filePrefix+"write", rendererFunc(fileWriteRender))
	registerRenderer(filePrefix+"grep", rendererFunc(fileGrepRender))
	registerRenderer(filePrefix+"list", rendererFunc(fileListRender))
	registerRenderer(filePrefix+"delete", rendererFunc(fileOtherRender))
	registerRenderer(filePrefix+"move", rendererFunc(fileOtherRender))
}
