package dun

import (
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// SessionStats returns the session-level counters for /context display.
// setSystemBreakdown replaces the context breakdown. Called twice per rebuild:
// once with estimates (immediate) and once with exact counts (when the
// tokenizer answers), so /context is never blank and never blocks setup.
func (h *Harness) setSystemBreakdown(bd SystemBreakdown) {
	h.noteMu.Lock()
	h.systemTokens = bd.Total
	h.systemParts = bd
	cb := h.cfg.OnSystemBreakdown
	h.noteMu.Unlock()
	// Outside the lock: this reaches the TUI, and a callback that blocks would
	// hold the mutex the queue counters share.
	if cb != nil {
		cb(bd)
	}
}

// SystemParts returns the pre-conversation context in parts, with Exact
// telling the caller whether the numbers were measured or estimated. The
// distinction is the point: rendering a guess like a measurement is the failure
// this replaced.
func (h *Harness) SystemParts() SystemBreakdown {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	return h.systemParts
}

// dun's coding-agent persona and what is not about one tool.
//
// Assembled per session rather than fixed, because the tool families are not
// fixed: code and docs are opt-in and can come and go mid-session. Describing a
// family whose tools are absent is worse than saying nothing — the model plans
// around node_query, calls it, and gets "unknown tool".
//
// Each fact is said ONCE (USER 2026-10-27: "reduce the verbosity and
// duplication"). What a dun tool does is in its own description; the system
// prompt keeps the role, the containment, how to choose between tools, how to
// work, and the lines for MCP servers whose descriptions dun does not write.
const (
	systemPreamble = "You are dun (" + Icon + "), a coding agent"

	// systemThinking makes reasoning skimmable: the TUI shows a thought as one
	// line, the latest heading while it streams and the summary after
	// (thinkHeadline). Fixed text, so it keeps the cache.
	systemThinking = " While you think, before you reply, title each step with a line `# Thinking: <what, in a few words>` and end with a line `# Summary: <what you concluded>`. These lines are for your private thinking only: never put them in your reply to the user."

	systemCode  = "\n- code (poly-lsp-mcp): node_query to find/navigate code by selector (call it with selector \"?\" to learn the grammar), node_read to read a symbol whole, node_edit to edit/rename/refactor. Edits return diagnostics."
	systemShell = "\n- shell (mcpshell): eval runs sandboxed script code for computation, data wrangling, and jailed file ops."
	systemDocs  = "\n- docs (raglit): search the document/knowledge index; get_document to read a full hit."

	systemDocsNote = "\n\nRelevant docs may be pushed to you as [docs] notes — use them."

	systemWorkCode = "\n\nWork step by step: find with node_query, read what you need, make minimal precise edits, verify via the diagnostics AND by running the build/tests with exec. Prefer node_edit over rewriting files. Be concise. When the task is done, briefly summarize what you changed."
	systemWork     = "\n\nWork step by step: read what you need before changing it, make minimal precise edits, and verify by running the build/tests with exec. Be concise. When the task is done, briefly summarize what you changed."

	// systemSlots replaces agentkit's SlotSystemNote (OmitSlotInstructions):
	// the same convention in two sentences instead of six.
	systemSlots = "\n\nTo show the user a tool's output verbatim, write {OUTPUT} (the latest tool result) or {name} " +
		"(a section the tool wrapped in <name>…</name>) instead of retyping it. The user sees the complete output " +
		"there, even when your view of it was clipped."
)

// systemExecCap quotes the clip limit, which must be the one number the code
// enforces. Only with exec: the ref it names is read with read_saved, and the
// clipping is exec's (execInlineMax).
//
// "Do not claim" stays: models reported what was in the elided middle.
var systemExecCap = "\n\nA tool result over " + fmt.Sprintf("%d", execInlineMax) + " characters reaches you clipped " +
	"to its start and end; the gap names a ref, and read_saved reads the rest. That is not a failure, and re-running " +
	"only clips it again. Do not claim to have seen the clipped part."

// HostedLine is the system prompt's sentence naming the servers hosted in
// the shell, "" for none: what scripting a server costs in every request
// (/mcp info prices it).
func HostedLine(hosted []string) string {
	if len(hosted) == 0 {
		return ""
	}
	return fmt.Sprintf(" MCP servers hosted in the shell, called inside eval as <ns>.<tool>({…}): %s — help(%q) lists one's commands.",
		strings.Join(hosted, ", "), hosted[0])
}

// systemFor describes only the tool families actually present. files is the
// file tools' line (systemFiles), "" when they are off. dun's own tools
// (exec, ask_user, recap, agent, jobs, session) are described by their
// definitions alone.
//
// hosted are the MCP servers the shell carries as eval namespaces. mcpshell's
// eval description is fixed text that names none of them, so without this
// line the model has no way to know they exist.
//
// bare: no client renders the reply or shows the thinking (--bare), so the
// heading convention for the TUI's thought line is left out.
func systemFor(tools []mcpmgr.MCPTool, exec ExecBackend, wt *Worktree, files string, bare bool, hosted ...string) string {
	have := map[string]bool{}
	for _, t := range tools {
		have[t.ServerID] = true
	}
	var b strings.Builder
	b.WriteString(systemPreamble)
	// Tell the agent about its containment so it knows what it's working with.
	// Only a real worktree (one with a branch of its own) isolates anything:
	// the pass-through one dun makes without --worktree is the directory
	// itself, and calling it a worktree told the model it was isolated when
	// it worked in place.
	var containment []string
	if wt != nil && wt.Path != "" && wt.Branch != "" {
		containment = append(containment, "git worktree at "+wt.Path)
	}
	if _, ok := exec.(DockerExec); ok {
		containment = append(containment, "Docker container")
	}
	if len(containment) > 0 {
		b.WriteString(", working inside an isolated workspace. Containment: " + strings.Join(containment, " + ") + ".")
	} else {
		b.WriteString(".")
	}
	if !bare {
		b.WriteString(systemThinking)
	}
	var fam strings.Builder
	if have[ServerCode] {
		fam.WriteString(systemCode)
	}
	if have[ServerShell] {
		fam.WriteString(systemShell)
		fam.WriteString(HostedLine(hosted))
	}
	if have[ServerDocs] {
		fam.WriteString(systemDocs)
	}
	if fam.Len() > 0 {
		b.WriteString("\n\nTool servers:" + fam.String())
	}
	b.WriteString(files)
	if exec != nil {
		b.WriteString(systemExecCap)
	}
	if have[ServerDocs] {
		b.WriteString(systemDocsNote)
	}
	if have[ServerCode] {
		b.WriteString(systemWorkCode)
	} else {
		b.WriteString(systemWork)
	}
	return b.String()
}
