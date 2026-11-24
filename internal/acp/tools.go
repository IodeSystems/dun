package acp

import (
	"fmt"
	"path/filepath"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// A dun tool call as an ACP tool call: its kind (the editor's icon), a
// title, the files it names (locations, the editor follows them), and at its
// result the text and the file edits as diff content.

// toolKinds maps dun's tool names to ACP's kinds. A name not here is
// "other"; an MCP server's tool ("<server>__<tool>") is looked up by its own
// name.
var toolKinds = map[string]sdk.ToolKind{
	"file_read": sdk.ToolKindRead, "node_read": sdk.ToolKindRead, "file_list": sdk.ToolKindRead,
	"file_edit": sdk.ToolKindEdit, "file_write": sdk.ToolKindEdit, "node_edit": sdk.ToolKindEdit, "node_refactor": sdk.ToolKindEdit,
	"file_delete": sdk.ToolKindDelete, "node_delete": sdk.ToolKindDelete,
	"file_move": sdk.ToolKindMove, "node_rename_file": sdk.ToolKindMove,
	"file_grep": sdk.ToolKindSearch, "node_query": sdk.ToolKindSearch, "search": sdk.ToolKindSearch,
	"docs_search": sdk.ToolKindSearch, "lookup": sdk.ToolKindSearch,
	"exec": sdk.ToolKindExecute, "eval": sdk.ToolKindExecute,
	"recap": sdk.ToolKindThink, "asks": sdk.ToolKindThink, "plan_add": sdk.ToolKindThink, "plan_update": sdk.ToolKindThink,
	"plan_close": sdk.ToolKindThink, "plan_show": sdk.ToolKindThink,
	"fetch": sdk.ToolKindFetch, "web_fetch": sdk.ToolKindFetch,
}

func toolKind(name string) sdk.ToolKind {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	if k, ok := toolKinds[name]; ok {
		return k
	}
	return sdk.ToolKindOther
}

// titleArgs are the arguments a title shows, the first one present.
var titleArgs = []string{"command", "path", "file", "node", "query", "pattern", "to", "question", "code"}

// toolTitle is the tool's name and its main argument, on one line.
func toolTitle(tool string, args map[string]any, cwd string) string {
	for _, k := range titleArgs {
		s, _ := args[k].(string)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if k == "path" || k == "file" || k == "to" {
			if rel, err := filepath.Rel(cwd, s); err == nil && filepath.IsAbs(s) && !strings.HasPrefix(rel, "..") {
				s = rel
			}
		}
		s, _, _ = strings.Cut(s, "\n")
		if r := []rune(s); len(r) > 80 {
			s = string(r[:79]) + "…"
		}
		return tool + ": " + s
	}
	return tool
}

// locationArgs are the arguments that name a file.
var locationArgs = []string{"path", "file", "to", "node"}

// toolLocations are the files a call's arguments name, absolute (a relative
// path is under cwd); a node ("file#Symbol") names its file.
func toolLocations(args map[string]any, cwd string) []sdk.ToolCallLocation {
	var out []sdk.ToolCallLocation
	seen := map[string]bool{}
	for _, k := range locationArgs {
		s, _ := args[k].(string)
		if k == "node" {
			s, _, _ = strings.Cut(s, "#")
		}
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s, "\n*?") {
			continue
		}
		if !filepath.IsAbs(s) {
			if cwd == "" {
				continue
			}
			s = filepath.Join(cwd, s)
		}
		s = filepath.Clean(s)
		if !seen[s] {
			seen[s] = true
			out = append(out, sdk.ToolCallLocation{Path: s})
		}
	}
	return out
}

// startCall is a tool_call update for a call that began.
func startCall(c *proto.ToolCall, cwd string) sdk.SessionUpdate {
	opts := []sdk.ToolCallStartOpt{
		sdk.WithStartKind(toolKind(c.Tool)),
		sdk.WithStartName(c.Tool),
		sdk.WithStartStatus(sdk.ToolCallStatusInProgress),
	}
	if l := toolLocations(c.Args, cwd); len(l) > 0 {
		opts = append(opts, sdk.WithStartLocations(l))
	}
	if c.Args != nil {
		opts = append(opts, sdk.WithStartRawInput(c.Args))
	}
	return sdk.StartToolCall(sdk.ToolCallId(c.CallID), toolTitle(c.Tool, c.Args, cwd), opts...)
}

// endCall is the tool_call_update for a call's result: completed or
// failed, its text, and a diff per file it changed.
func endCall(r *proto.ToolResult) sdk.SessionUpdate {
	status := sdk.ToolCallStatusCompleted
	if r.IsError {
		status = sdk.ToolCallStatusFailed
	}
	return sdk.UpdateToolCall(sdk.ToolCallId(r.CallID),
		sdk.WithUpdateStatus(status),
		sdk.WithUpdateContent(resultContent(r.Result, r.Edits)))
}

// resultContent is a result's text, then its edits: a diff for each file
// with texts, a line of text for each one whose texts were left out.
func resultContent(result string, edits []proto.FileEdit) []sdk.ToolCallContent {
	var out []sdk.ToolCallContent
	if strings.TrimSpace(result) != "" {
		out = append(out, sdk.ToolContent(sdk.TextBlock(result)))
	}
	for _, e := range edits {
		switch {
		case e.Skipped != "":
			out = append(out, sdk.ToolContent(sdk.TextBlock(fmt.Sprintf("%s changed; no diff (%s)", e.Path, e.Skipped))))
		case e.Created:
			out = append(out, sdk.ToolDiffContent(e.Path, e.NewText))
		default:
			out = append(out, sdk.ToolDiffContent(e.Path, e.NewText, e.OldText))
		}
	}
	return out
}
