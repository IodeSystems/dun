package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/agent"

	"github.com/iodesystems/dun/internal/memory"
	"github.com/iodesystems/dun/internal/project"
	"github.com/iodesystems/dun/internal/raglit"
)

// The session's memory notes, strategies and constraints (internal/memory,
// docs/memory.md, docs/strategies.md): the stores a tool rebuild makes, the
// links to dund they index through, and the glue into the proactive search
// (notify.go). What a note is, and how it is written, found and reviewed,
// is the memory package's.

// memoryStore is the note store, or nil when the session has no RagLink.
func (h *Harness) memoryStore() *memory.Store { return h.memory.Load() }

// strategyStore is the strategies and constraints store, or nil.
func (h *Harness) strategyStore() *memory.Store { return h.strategies.Load() }

// memStores is the session's two stores, as they are now.
func (h *Harness) memStores() memory.Stores {
	return memory.Stores{Notes: h.memoryStore(), Strategies: h.strategyStore()}
}

// memoryCall parses and applies one memory call, rendering any error as the
// result the model reads.
func (h *Harness) memoryCall(m *memory.Store, rawArgs string) string {
	s := h.memStores()
	before := s.ImportantKey()
	out := s.Call(m, rawArgs, h.noteMeta())
	// An important item changed or went: the standing list in the system
	// prompt is rebuilt (after the turn, applyTools).
	if s.ImportantKey() != before {
		h.applyTools()
	}
	return out
}

// noteMeta is where a write from this session says it came from.
func (h *Harness) noteMeta() memory.NoteMeta {
	meta := memory.NoteMeta{Commit: project.GitLine(h.cfg.Workspace, "rev-parse", "--short", "HEAD")}
	if h.cfg.SessionFile != "" {
		meta.Session = strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
	}
	return meta
}

// Memories lists this project's notes and the global ones, then the
// strategies and constraints, or nil when the session keeps none (no
// RagLink).
func (h *Harness) Memories() []memory.Info { return h.memStores().List() }

// Remembering reports whether notes can be written and found: the session
// has a RagLink (dund, or with dund down the notes on disk).
func (h *Harness) Remembering() bool { return h.memoryStore() != nil }

// MemoriesCommand carries out /memories add, rm, important, unimportant,
// candidates, accept and reject (memory.Stores.Command), and returns the
// line for the user.
func (h *Harness) MemoriesCommand(action string) (string, error) {
	s := h.memStores()
	before := s.ImportantKey()
	defer func() {
		if s.ImportantKey() != before {
			h.applyTools()
		}
	}()
	return s.Command(action, h.noteMeta())
}

// standingBlock is the system prompt's list of important items, and sets
// the files it lists (which recall then leaves out). "" when none.
func (h *Harness) standingBlock() string {
	block, listed := h.memStores().Standing()
	h.listed.Store(&listed)
	return block
}

// listedImportant says the standing list shows path.
func (h *Harness) listedImportant(path string) bool {
	if l := h.listed.Load(); l != nil {
		return (*l)[path]
	}
	return false
}

// fetchNote reads a note whole for inlining: the file, when it is at most
// max chars.
func fetchNote(path string, max int) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > max {
		return "", false
	}
	return string(b), true
}

// memLink is the harness's link for its notes, nil without a RagLink. A
// child shares its parent's, so a sub-agent does not sync the notes again.
func (h *Harness) memLink() *memory.Link {
	h.memLinkOnce.Do(func() {
		h.memLinkV = h.newStoreLink(func(p *Harness) *memory.Link { return p.memLink() }, memory.NotesFor, h.cfg.OnNotify)
	})
	return h.memLinkV
}

// stratLink is the same for the strategies and constraints: its own link,
// so each index syncs on its own. It gives no notice: the notes' link says
// dund is down for both.
func (h *Harness) stratLink() *memory.Link {
	h.stratLinkOnce.Do(func() {
		h.stratLinkV = h.newStoreLink(func(p *Harness) *memory.Link { return p.stratLink() }, memory.StrategiesFor, nil)
	})
	return h.stratLinkV
}

func (h *Harness) newStoreLink(parent func(*Harness) *memory.Link, store func(*memory.Link, string) *memory.Store, notice func(string)) *memory.Link {
	if h.cfg.Rag == nil {
		return nil
	}
	if p := h.cfg.Parent; p != nil {
		if l := parent(p); l != nil {
			return l
		}
	}
	reg := h.cfg.RaglitReg
	// The link's dund index is keyed by the workspace's registry id, the
	// same key NotesFor keys the notes' directory by — the directory and
	// the index must agree, or a note is filed under one and searched under
	// the other. (The raglit layer it also uploads to is keyed by the
	// registration's own name, not this.)
	proj := memory.KeyFor(h.cfg.Workspace)
	var l *memory.Link
	if reg.Private != "" {
		// A registered remote project: notes upload to their layer on
		// write (internal/memory), raglit.New reading $RAGLIT_TOKEN per
		// request (the /rag login keeps it there).
		l = memory.NewLinkWithRaglit(h.cfg.Rag, proj, notice, raglit.New(""), reg)
	} else {
		l = memory.NewLink(h.cfg.Rag, proj, notice)
	}
	// The first contact: files written while no dund was there get
	// indexed now, or the session learns it is degraded.
	go l.Contact(store(l, h.cfg.Workspace))
	return l
}

// withMemFinder adds the memory finder to the proactive search: its hits
// beside the docs server's (inner, nil without one), read whole from their
// files, and described from what dund said of them. src is changed in
// place to look a note up here first.
func withMemFinder(mf *memory.Finder, inner agent.DocFinder, fetch docBodyFunc, src *hitSources) (agent.DocFinder, docBodyFunc) {
	index, meta, meaning := src.index, src.meta, src.meaning
	src.index = func(docID string) string {
		if h, ok := mf.Hit(docID); ok {
			return h.Index
		}
		if index != nil {
			return index(docID)
		}
		return ""
	}
	src.meta = func(docID string) (hitMeta, bool) {
		if h, ok := mf.Hit(docID); ok {
			return hitMeta(h.Meta), true
		}
		if meta != nil {
			return meta(docID)
		}
		return hitMeta{}, false
	}
	src.meaning = func(docID string) (bool, bool) {
		if h, ok := mf.Hit(docID); ok {
			return h.Meaning, true
		}
		if meaning != nil {
			return meaning(docID)
		}
		return false, false
	}
	readAll := func(ctx context.Context, docID string, max int) (string, bool) {
		if mf.Holds(docID) {
			return fetchNote(docID, max)
		}
		if fetch != nil {
			return fetch(ctx, docID, max)
		}
		return "", false
	}
	if inner == nil {
		return mf, readAll
	}
	return bothFinders{mf, inner}, readAll
}

// bothFinders asks the memory finder and the docs server's at once. One
// failing leaves the other's hits; both failing is the docs server's error.
type bothFinders struct {
	mem, docs agent.DocFinder
}

func (b bothFinders) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	var memHits []agent.DocHit
	var memErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		memHits, memErr = b.mem.Find(ctx, texts)
	}()
	docHits, docErr := b.docs.Find(ctx, texts)
	<-done
	if memErr != nil && docErr != nil {
		return nil, docErr
	}
	return append(memHits, docHits...), nil
}
