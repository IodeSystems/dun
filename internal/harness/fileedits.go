package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"
)

// File edits: what a tool call changed on disk, read by dun before and after
// the call, for clients (an ACP bridge's diff content, the TUI). The model is
// not sent them: its result is the tool's own text, as before. The tools read:
// dun's own file tools that write (edit, write, delete, move: path, and a
// move's to) and poly-lsp's node_edit and node_delete (file, or the file of
// node "<file>#<sym>"). Not read: exec and mcpshell's eval (a command or a
// script can change any file), node_refactor and node_rename_file (they
// rewrite files across the workspace that their arguments do not name).
// docs/event-protocol.md, "File edits".

const (
	// editFileMax: a file larger than this, before or after the call, is
	// named without its texts (Skipped "too_large").
	editFileMax = 1 << 20
	// editCallMax: the texts one call carries, its files together; past it
	// the rest are named without texts (Skipped "call_cap").
	editCallMax = 2 << 20
	// editHistoryMax: the texts a history replay carries, newest call first;
	// older edits are named without texts (Skipped "history_cap").
	editHistoryMax = 8 << 20
)

// FileEdit is one file a tool call changed (proto.FileEdit). Path is
// absolute. Skipped says why the texts are left out.
type FileEdit struct {
	Path    string `json:"path"`
	OldText string `json:"old_text,omitempty"`
	NewText string `json:"new_text,omitempty"`
	Created bool   `json:"created,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

func (e FileEdit) size() int { return len(e.OldText) + len(e.NewText) }

// withTexts drops the texts and says why.
func (e FileEdit) withoutTexts(why string) FileEdit {
	e.OldText, e.NewText, e.Skipped = "", "", why
	return e
}

// fileSnap is a path as read at one moment. sig tells two reads apart: the
// text's hash, or the size and time of a file whose text is not kept.
type fileSnap struct {
	exists  bool
	text    string
	sig     string
	skipped string
}

func snapFile(abs string) fileSnap {
	st, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSnap{}
	}
	if err != nil {
		return fileSnap{exists: true, skipped: "unreadable"}
	}
	statSig := fmt.Sprintf("%d %d %v", st.Size(), st.ModTime().UnixNano(), st.Mode())
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return fileSnap{exists: true, sig: statSig, skipped: "symlink"}
	case st.IsDir():
		return fileSnap{exists: true, sig: statSig, skipped: "directory"}
	case !st.Mode().IsRegular():
		return fileSnap{exists: true, sig: statSig, skipped: "unreadable"}
	case st.Size() > editFileMax:
		return fileSnap{exists: true, sig: statSig, skipped: "too_large"}
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return fileSnap{exists: true, sig: statSig, skipped: "unreadable"}
	}
	if len(b) > editFileMax { // grew between the stat and the read
		return fileSnap{exists: true, sig: statSig, skipped: "too_large"}
	}
	sum := sha256.Sum256(b)
	sig := string(sum[:])
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return fileSnap{exists: true, sig: sig, skipped: "binary"}
	}
	return fileSnap{exists: true, text: string(b), sig: sig}
}

// diffSnap is the edit from before to after at path; ok=false when the path
// did not change.
func diffSnap(path string, before, after fileSnap) (FileEdit, bool) {
	if !before.exists && !after.exists {
		return FileEdit{}, false
	}
	if before.exists && after.exists && before.sig == after.sig && before.skipped == after.skipped {
		return FileEdit{}, false
	}
	e := FileEdit{Path: path, Created: !before.exists, Deleted: !after.exists}
	if why := cmpOr(before.skipped, after.skipped); why != "" {
		e.Skipped = why
		return e, true
	}
	e.OldText, e.NewText = before.text, after.text
	return e, true
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// capCall keeps the texts of a call's edits within editCallMax, in order.
func capCall(edits []FileEdit) []FileEdit {
	total := 0
	for i, e := range edits {
		if total+e.size() > editCallMax {
			edits[i] = e.withoutTexts("call_cap")
			continue
		}
		total += e.size()
	}
	return edits
}

// editPaths are the absolute paths a call may change, when its tool is one
// dun reads (see the top of this file); none for any other call. ft is the
// session's file toolset (nil when the file tools are off); a relative path
// is under its root, and a poly-lsp one under the workspace.
func editPaths(ft *filetools.Toolset, workspace, name, args string) []string {
	var raw []string
	root := workspace
	switch {
	case ft != nil && ft.Handles(name):
		if !writesFiles(ft, name) {
			return nil
		}
		root = ft.Root()
		for _, k := range pathKeys(ft, name) {
			raw = append(raw, argPath(args, k))
		}
	case name == "node_edit" || name == "node_delete":
		p := argPath(args, "file")
		if p == "" {
			p, _, _ = strings.Cut(argPath(args, "node"), "#")
		}
		raw = append(raw, strings.TrimSpace(p))
	default:
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range raw {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			if root == "" {
				continue
			}
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// withEdits reads the files a call may change before and after it, and
// hands what changed to the call's tool_result event (toolCur) and the
// session's edits sidecar. Only when a client hears tool results: a
// sub-agent's calls and an embedder without OnToolEnd are not read.
func withEdits(inner agent.ToolDispatcher, h *Harness, ft *filetools.Toolset) agent.ToolDispatcher {
	if h.cfg.OnToolEnd == nil {
		return inner
	}
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		paths := editPaths(ft, h.Workspace(), tc.Function.Name, tc.Function.Arguments)
		if len(paths) == 0 {
			return inner(ctx, tc)
		}
		before := make([]fileSnap, len(paths))
		for i, p := range paths {
			before[i] = snapFile(p)
		}
		out, err := inner(ctx, tc)
		var edits []FileEdit
		for i, p := range paths {
			if e, ok := diffSnap(p, before[i], snapFile(p)); ok {
				edits = append(edits, e)
			}
		}
		if len(edits) > 0 {
			edits = capCall(edits)
			h.toolCur.noteEdits(edits)
			if h.store != nil {
				h.store.saveEdits(tc.ID, edits)
			}
		}
		return out, err
	}
}

// ── the edits sidecar ─────────────────────────────────────────────

// "<id>.edits.jsonl": one line per call that changed files, {call_id,
// edits}, appended. A text over blobThreshold is a blob ref (blobs/ is
// content-addressed: one version of a file edited twice is kept once). It
// goes with the session (removeSidecars: "<id>.*.jsonl"), and /clear removes
// it with the transcript it answered for.

type editsLine struct {
	CallID string     `json:"call_id"`
	Edits  []FileEdit `json:"edits"`
}

func (s *sessionStore) editsPath() string {
	if s.path == "" {
		return ""
	}
	return strings.TrimSuffix(s.path, ".jsonl") + ".edits.jsonl"
}

// saveEdits appends a call's edits to the sidecar. Best effort: a failed
// write loses the edits' replay, never the call.
func (s *sessionStore) saveEdits(callID string, edits []FileEdit) {
	p := s.editsPath()
	if p == "" || callID == "" {
		return
	}
	stored := make([]FileEdit, len(edits))
	for i, e := range edits {
		for _, t := range []*string{&e.OldText, &e.NewText} {
			if len(*t) > blobThreshold {
				if ref, ok := s.writeBlob(*t); ok {
					*t = ref
				}
			}
		}
		stored[i] = e
	}
	line, err := json.Marshal(editsLine{CallID: callID, Edits: stored})
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// edits reads the sidecar: each call's edits by call id, blob refs read
// back. A blob that is gone leaves its edit named without texts.
func (s *sessionStore) edits() map[string][]FileEdit {
	p := s.editsPath()
	if p == "" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string][]FileEdit{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4*editCallMax)
	for sc.Scan() {
		var l editsLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.CallID == "" {
			continue
		}
		for i, e := range l.Edits {
			lost := false
			for _, t := range []*string{&e.OldText, &e.NewText} {
				if strings.HasPrefix(*t, blobMarker) {
					full, ok := s.readBlob(*t)
					*t, lost = full, lost || !ok
				}
			}
			if lost {
				e = e.withoutTexts("unreadable")
			}
			l.Edits[i] = e
		}
		out[l.CallID] = l.Edits
	}
	return out
}

// attachEdits puts each tool_result's edits on its history item, the newest
// first within editHistoryMax.
func attachEdits(items []HistoryItem, byCall map[string][]FileEdit) []HistoryItem {
	if len(byCall) == 0 {
		return items
	}
	budget := editHistoryMax
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind != "tool_result" || it.CallID == "" {
			continue
		}
		edits := byCall[it.CallID]
		if len(edits) == 0 {
			continue
		}
		out := make([]FileEdit, len(edits))
		for j, e := range edits {
			if e.size() > budget {
				e = e.withoutTexts("history_cap")
			} else {
				budget -= e.size()
			}
			out[j] = e
		}
		items[i].Edits = out
	}
	return items
}
