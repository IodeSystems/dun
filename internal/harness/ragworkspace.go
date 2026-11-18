package harness

import (
	"fmt"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// /rag workspace off|on (USER 2026-10-27): raglit for other indexes
// (attached ones, memory, skills) without reading this workspace into an
// index. Off, the workspace.open ingest is skipped; search still reads the
// project's own indexes and the attached ones. Kept at the scopes of the
// other switches (switches.go) in rag.json under "workspace"; on unless a
// scope says off. A --no-dir session has no workspace to read: always off.
// With the code and docs search through dund on (codesearch.go), raglit
// never reads the workspace: it is dund's, and this switch has no effect.

const ragKind = "rag"

// WorkspaceIndexed is whether raglit reads this workspace into its index,
// and the scope that decided ("" the default, "--no-dir").
func (h *Harness) WorkspaceIndexed() (bool, string) {
	if h.cfg.NoDir {
		return false, "--no-dir"
	}
	on, from, _ := h.switchSet(ragKind).on("workspace")
	return on, from
}

// DundReadsWorkspace is whether dund indexes this workspace into the
// project index (codesearch.go): then raglit never reads it, and the
// workspace switch has no effect.
func (h *Harness) DundReadsWorkspace() bool { return h.codeLink() != nil }

// SetWorkspaceIndexed sets it at scope; the answer is the message shown.
func (h *Harness) SetWorkspaceIndexed(on bool, scope string) (string, error) {
	if h.cfg.NoDir {
		return "", fmt.Errorf("a --no-dir session has no workspace to index")
	}
	if err := h.editSwitchFile(ragKind, scope, func(f *SwitchFile) {
		if f.On == nil {
			f.On = map[string]bool{}
		}
		f.On["workspace"] = on
	}); err != nil {
		return "", err
	}
	now, from := h.WorkspaceIndexed()
	msg := fmt.Sprintf("workspace indexing %s for %s", mcpserver.OnOffWord(on), scopeWords(scope))
	if now != on {
		msg += fmt.Sprintf(" — but %s still says %s", from, mcpserver.OnOffWord(now))
	}
	if h.DundReadsWorkspace() {
		msg += " — no effect now: dund reads this workspace (always), and raglit does not"
	}
	return msg, nil
}
