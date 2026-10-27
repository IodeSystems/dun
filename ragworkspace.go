package dun

import "fmt"

// /raglit workspace off|on (USER 2026-10-27): raglit for other indexes
// (attached ones, memory, skills) without reading this workspace into an
// index. Off, the workspace.open ingest is skipped; search still reads the
// project's own indexes and the attached ones. Kept at the scopes of the
// other switches (switches.go) in rag.json under "workspace"; on unless a
// scope says off. A --no-dir session has no workspace to read: always off.

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
	msg := fmt.Sprintf("workspace indexing %s %s", onOffWord(on), scopeWords(scope))
	if now != on {
		msg += fmt.Sprintf(" — but %s still says %s", from, onOffWord(now))
	}
	return msg, nil
}
