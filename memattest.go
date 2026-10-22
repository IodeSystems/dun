package dun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A note's history (plan item 9 M4).
//
// The note file is always the CURRENT answer — that is what the index serves.
// What happened to it lives beside it in <note>.md.attest.jsonl, append-only,
// one line per verdict, in the shape of raglit's attest log (attest.Entry:
// kind, unit, text, note, by, at), so raglit's tooling can read it. One
// field is dun's own: "was", the text a refine replaced, because the note
// file is rewritten and attest's reading file, which would hold it, is not
// kept for notes.
//
//	refine   corrected    the answer changed; text is the new one
//	void     unsupported  it no longer applies; the file becomes *.md.void
//	confirm  confirmed    checked against the code or the user, and right
//
// unit is a content id of the question and answer ruled on. A confirmation
// whose unit is not the note's current one was made before a later change:
// "confirmed, then changed" is a comparison, not something anyone has to
// notice (raglit/plan/answered-questions.md, Orphaned in attest/state.go).
//
// Refines and voids are user- or reality-sourced (USER, 2026-10-21): source
// is "user" or "code", and they apply without asking.

const (
	attestSuffix = ".attest.jsonl"
	voidSuffix   = ".void"
)

type attestEntry struct {
	Kind string `json:"kind"`
	Unit string `json:"unit,omitempty"`
	Text string `json:"text,omitempty"`
	Note string `json:"note,omitempty"`
	By   string `json:"by"`
	At   string `json:"at,omitempty"`
	Was  string `json:"was,omitempty"`
}

// attestBy is who a verdict is from: the user (relayed by the model), or
// the code as dun read it.
func attestBy(a memoryArgs) (string, error) {
	if strings.TrimSpace(a.Why) == "" {
		return "", fmt.Errorf("why is required: what the user said, or what in the code shows it")
	}
	switch a.Source {
	case "user":
		return "user", nil
	case "code":
		return "dun:code", nil
	}
	return "", fmt.Errorf("source must be user or code, not %q", a.Source)
}

func (m *memoryStore) attest(notePath string, e attestEntry) error {
	e.At = m.now().UTC().Format("2006-01-02T15:04:05Z")
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(notePath+attestSuffix, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func readAttest(notePath string) []attestEntry {
	b, err := os.ReadFile(notePath + attestSuffix)
	if err != nil {
		return nil
	}
	var out []attestEntry
	for _, l := range strings.Split(string(b), "\n") {
		var e attestEntry
		if json.Unmarshal([]byte(l), &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out
}

// note is a parsed note file.
type note struct {
	meta map[string]string
	body string
}

func readNote(path string) (note, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return note{}, err
	}
	meta, body := splitFrontmatter(string(b))
	return note{meta: meta, body: strings.TrimSpace(body)}, nil
}

// unit is the note's content id: its question and answer.
func (n note) unit() string {
	sum := sha256.Sum256([]byte(n.meta["question"] + "\n" + n.body))
	return hex.EncodeToString(sum[:8])
}

// MemoryInfo is one note, for /memories.
type MemoryInfo struct {
	File      string `json:"file"`
	Question  string `json:"question"`
	Body      string `json:"body"`
	Date      string `json:"date"`
	Refined   int    `json:"refined,omitempty"`
	Confirmed string `json:"confirmed,omitempty"`
	// StaleConfirm: confirmed once, then changed — the confirmation was of
	// text the note no longer says.
	StaleConfirm bool   `json:"staleConfirm,omitempty"`
	Void         bool   `json:"void,omitempty"`
	VoidWhy      string `json:"voidWhy,omitempty"`
}

// list reports every note, current first by date, then the voided ones.
func (m *memoryStore) list() []MemoryInfo {
	var out []MemoryInfo
	for _, pat := range []string{"*.md", "*.md" + voidSuffix} {
		paths, _ := filepath.Glob(filepath.Join(m.dir, pat))
		sort.Sort(sort.Reverse(sort.StringSlice(paths)))
		for _, p := range paths {
			n, err := readNote(p)
			if err != nil {
				continue
			}
			base := strings.TrimSuffix(p, voidSuffix)
			info := MemoryInfo{File: filepath.Base(base), Question: n.meta["question"], Body: n.body,
				Date: n.meta["date"], Void: strings.HasSuffix(p, voidSuffix)}
			cur := n.unit()
			for _, e := range readAttest(base) {
				switch e.Kind {
				case "corrected":
					info.Refined++
				case "confirmed":
					if e.Unit == cur {
						info.Confirmed, info.StaleConfirm = e.At[:10], false
					} else if info.Confirmed == "" {
						info.StaleConfirm = true
					}
				case "unsupported":
					info.VoidWhy = e.Note
				}
			}
			out = append(out, info)
		}
	}
	return out
}

// Memories lists this project's notes, or nil without the docs index.
func (h *Harness) Memories() []MemoryInfo {
	m := h.memoryStore()
	if m == nil {
		return nil
	}
	return m.list()
}

// Remembering reports whether notes can be written and found: the docs
// index is up.
func (h *Harness) Remembering() bool { return h.memoryStore() != nil }
