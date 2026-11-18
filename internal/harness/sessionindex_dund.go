package harness

// sessionindex_dund.go — folded history in dund's sessions index.
//
// What a fold or a recap takes out of the context (agentkit's SessionIndex)
// is written to a file beside the session log, <session>.folds/<key>.md, and
// the file is ingested into the project's sessions index in dund, tagged
// session:<id> and kind:<fold|recap> (USER 2026-11-17: "dund default, raglit
// add-on"). session_search reads it back, filtered to the session's tag. A
// sub-agent's folds go under its root's session (ragRoot): one session, one
// history.
//
// The file is the record, and the only copy of what the fold deleted. The
// whole directory is synced (rag_sync) at each Put and when the session
// starts or resumes, so a span written while dund was down is indexed at the
// next of those. The files go with the session: /close removes them
// (ForgetSession) and their documents (ForgetSessionFolds); a prune removes
// only the documents, as it keeps a session's other archives
// (pruneTranscript). A forget dund could not take is kept in a pending list
// beside the session logs and sent again at the next sync or forget that
// reaches dund. /clear keeps the session's id, so the spans of the log it
// set aside stay with the session, as that log does.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/proto"
)

// sessionsIndex is dund's per-project index of folded history (ragd).
const sessionsIndex = "sessions"

// errOldDund is a dund from before the sessions index, which refuses it by
// name (ragd.Name: `no index "sessions"`).
var errOldDund = errors.New("this dund predates the sessions index: restart dund")

func isOldDund(err error) bool {
	return err != nil && strings.Contains(err.Error(), `no index "sessions"`)
}

// dundSessionIndex is one root session's folded history in dund.
type dundSessionIndex struct {
	link    RagLink
	project string
	session string
	dir     string       // <session>.folds
	logs    string       // the directory of the session logs: the pending forgets
	notice  func(string) // says a failure, once until a success

	syncMu sync.Mutex     // one directory sync at a time
	syncs  sync.WaitGroup // the syncs in flight, which Close waits on
	mu     sync.Mutex
	said   string // the failure last said; "" after a success
}

// foldsDir is the directory of a session log's folded spans.
func foldsDir(sessionFile string) string {
	if abs, err := filepath.Abs(sessionFile); err == nil {
		sessionFile = abs
	}
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".folds"
}

// sessionIndex is the root session's index: nil without a dund link
// (DUN_MEMORY=0) or a session log to keep the spans beside.
func (h *Harness) sessionIndex() *dundSessionIndex {
	r := h.ragRoot()
	r.sessIdxOnce.Do(func() {
		if r.cfg.Rag == nil || r.cfg.SessionFile == "" {
			return
		}
		dir := foldsDir(r.cfg.SessionFile)
		r.sessIdx = &dundSessionIndex{link: r.cfg.Rag, project: raglitProject(r.cfg.Workspace),
			session: r.raglitSession(), dir: dir, logs: filepath.Dir(dir), notice: r.cfg.OnNotify}
	})
	return r.sessIdx
}

func (x *dundSessionIndex) tag() string { return "session:" + x.session }

// Put writes the span's file now and syncs the directory beside the turn.
func (x *dundSessionIndex) Put(ctx context.Context, d agent.SessionDoc) error {
	if err := x.write(d); err != nil {
		x.report(err)
		return err
	}
	x.syncAsync(context.WithoutCancel(ctx))
	return nil
}

// startSync is the sync at a session's start or resume: spans written while
// dund was down, and forgets it did not take. Nothing when there are none.
func (x *dundSessionIndex) startSync() {
	_, errDir := os.Stat(x.dir)
	_, errPending := os.Stat(pendingForgetFile(x.logs))
	if errDir != nil && errPending != nil {
		return
	}
	x.syncAsync(context.Background())
}

func (x *dundSessionIndex) syncAsync(ctx context.Context) {
	x.syncs.Add(1)
	go func() {
		defer x.syncs.Done()
		x.sync(ctx)
	}()
}

// sessionSyncGrace bounds Close's wait for a sync in flight.
const sessionSyncGrace = 5 * time.Second

// wait is Close's: the syncs in flight get up to d to finish, so a one-shot
// run that folded does not exit before its spans reached dund.
func (x *dundSessionIndex) wait(d time.Duration) {
	done := make(chan struct{})
	go func() { x.syncs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// The lines of a span file before its summary: the title, then these.
const (
	foldKind = "kind: "
	foldKey  = "key: "
)

func (x *dundSessionIndex) write(d agent.SessionDoc) error {
	if err := os.MkdirAll(x.dir, 0o700); err != nil {
		return err
	}
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = d.Key
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s%s\n%s%s\n\n## Summary\n\n%s\n\n## Span\n\n%s", oneLine(name, 200),
		foldKind, d.Kind, foldKey, d.Key, strings.TrimSpace(d.Summary), d.Body)
	// Whole or not at all: a sync never reads a half-written span. The temp
	// name does not end in .md, so the sync's glob skips it.
	f, err := os.CreateTemp(x.dir, ".span-*.tmp")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(x.dir, foldFileName(d.Key))); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}

// foldFileName is key as a file name, one name per key: a byte that is not
// a letter, digit, '-' or '_' (and a leading '.') is %XX; the empty key is
// "%empty", which no escaped key spells (escapes are upper-case hex).
func foldFileName(key string) string {
	if key == "" {
		return "%empty.md"
	}
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' && i > 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String() + ".md"
}

// sync sends every span file of the session to dund: the ones it has are
// current and cost a hash, the rest are queued. Then the pending forgets.
func (x *dundSessionIndex) sync(ctx context.Context) {
	x.syncMu.Lock()
	defer x.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, ragReplyTimeout)
	defer cancel()
	err := x.syncFiles(ctx)
	if err == nil {
		retryForgets(ctx, x.link, x.logs)
	}
	x.report(err)
}

// syncFiles is one rag_sync of the directory. A file gone between the glob
// and dund's read of it (a /close) fails the whole call in dund, so the
// directory is read again once and the call made again.
func (x *dundSessionIndex) syncFiles(ctx context.Context) error {
	var err error
	for range 2 {
		files := x.spanFiles()
		if len(files) == 0 {
			return nil
		}
		_, err = x.link.Sync(ctx, proto.RagSync{Project: x.project, Index: sessionsIndex, Files: files})
		if !isNotExist(err) {
			return err
		}
	}
	return err
}

func isNotExist(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "no such file"))
}

// spanFiles is the directory's span files that are there now, with their
// titles and tags.
func (x *dundSessionIndex) spanFiles() []proto.RagSyncFile {
	paths, _ := filepath.Glob(filepath.Join(x.dir, "*.md"))
	files := make([]proto.RagSyncFile, 0, len(paths))
	for _, p := range paths {
		hd, ok := foldHead(p)
		if !ok {
			continue // gone since the glob
		}
		tags := []string{x.tag()}
		if hd.kind != "" {
			tags = append(tags, "kind:"+hd.kind)
		}
		files = append(files, proto.RagSyncFile{Path: p, Title: hd.title, Tags: tags})
	}
	return files
}

// spanHead is what write puts before a span's summary.
type spanHead struct{ title, kind, key string }

// foldHead is a span file's head; ok is false when it cannot be read.
func foldHead(path string) (spanHead, bool) {
	var hd spanHead
	f, err := os.Open(path)
	if err != nil {
		return hd, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; i < 5 && sc.Scan(); i++ {
		ln := sc.Text()
		switch {
		case i == 0:
			hd.title = strings.TrimPrefix(ln, "# ")
		case strings.HasPrefix(ln, foldKind):
			hd.kind = strings.TrimSpace(strings.TrimPrefix(ln, foldKind))
		case strings.HasPrefix(ln, foldKey):
			hd.key = strings.TrimPrefix(ln, foldKey)
		}
	}
	return hd, true
}

// report says a failure once, until a success: a span that is not indexed
// is never dropped without a word (it stays in its file).
func (x *dundSessionIndex) report(err error) {
	msg := ""
	switch {
	case err == nil:
	case errors.Is(err, ErrRagDown):
		msg = "session history: dund is not reachable — folded history is kept in " + x.dir +
			" and indexed at the next fold, or the next start of this session, once dund is back; " +
			"session_search cannot read it until then"
	case isOldDund(err):
		msg = "session history: not indexed: " + errOldDund.Error() + " — folded history is kept in " + x.dir
	default:
		msg = "session history: not indexed: " + oneLine(err.Error(), 300) + " — folded history is kept in " + x.dir
	}
	x.mu.Lock()
	say := msg != "" && msg != x.said
	x.said = msg
	x.mu.Unlock()
	if say {
		log.Printf("dun: %s", msg)
		if x.notice != nil {
			x.notice(msg)
		}
	}
}

// Search is this session's spans that match q, best first.
func (x *dundSessionIndex) Search(ctx context.Context, q string, n int) ([]agent.SessionHit, error) {
	r, err := x.link.Search(ctx, proto.RagSearch{Project: x.project, Index: sessionsIndex, Query: q, Limit: n,
		Filter: proto.RagFilter{Tags: []string{x.tag()}}})
	switch {
	case errors.Is(err, ErrRagDown):
		return nil, fmt.Errorf("dund is not reachable; the folded spans are files in %s", x.dir)
	case isOldDund(err):
		return nil, errOldDund
	case err != nil:
		return nil, err
	}
	out := make([]agent.SessionHit, 0, len(r.Hits))
	for _, hit := range r.Hits {
		path := strings.TrimPrefix(hit.Path, "file://")
		key := strings.TrimSuffix(filepath.Base(path), ".md")
		if hd, ok := foldHead(path); ok && hd.key != "" {
			key = hd.key // the key as agentkit named it, not its file name
		}
		out = append(out, agent.SessionHit{Key: key, Name: hit.Title, Snippet: hit.Text, Score: hit.Score})
	}
	return out, nil
}

// Close leaves the spans: they go with the session (ForgetSessionFolds), not
// with the process.
func (x *dundSessionIndex) Close(context.Context) error { return nil }

// ForgetSessionFolds removes session id's folded history from dund's
// sessions index; the span files stay (ForgetSession removes them). root is
// the workspace the session belongs to (SessionFile). A forget dund does not
// take (down, or no link) is kept in the pending list and sent again at the
// next sync or forget that reaches dund; the error says so.
func ForgetSessionFolds(link RagLink, root, id string) error {
	logs := RootDir(root)
	paths, _ := filepath.Glob(filepath.Join(foldsDir(SessionFile(root, id)), "*.md"))
	ctx, cancel := context.WithTimeout(context.Background(), ragReplyTimeout)
	defer cancel()
	if len(paths) == 0 {
		if link != nil {
			retryForgets(ctx, link, logs)
		}
		return nil
	}
	project := raglitProject(root)
	err := errors.New("no dund link (DUN_MEMORY=0)")
	if link != nil {
		_, err = link.Sync(ctx, proto.RagSync{Project: project, Index: sessionsIndex, Gone: paths})
	}
	if err != nil {
		if perr := addPendingForgets(logs, project, paths); perr != nil {
			return fmt.Errorf("%w; and not kept to retry: %v", err, perr)
		}
		return fmt.Errorf("%w; kept to retry when dund is reachable", err)
	}
	retryForgets(ctx, link, logs)
	return nil
}

// pendingForgetFile lists the span documents dund still holds for sessions
// that are gone: one JSON object per line, {project, path}.
func pendingForgetFile(logs string) string { return filepath.Join(logs, "folds-forget.jsonl") }

type pendingForget struct {
	Project string `json:"project"`
	Path    string `json:"path"`
}

// pendingMu orders this process's edits of a pending list.
var pendingMu sync.Mutex

func addPendingForgets(logs, project string, paths []string) error {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	f, err := os.OpenFile(pendingForgetFile(logs), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, p := range paths {
		if err := enc.Encode(pendingForget{Project: project, Path: p}); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// retryForgets sends the pending list, per project, and keeps only what dund
// did not take; the file goes when nothing is left.
func retryForgets(ctx context.Context, link RagLink, logs string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	file := pendingForgetFile(logs)
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	by := map[string][]string{}
	var order []string
	for _, ln := range strings.Split(string(b), "\n") {
		var p pendingForget
		if json.Unmarshal([]byte(ln), &p) != nil || p.Path == "" {
			continue
		}
		if by[p.Project] == nil {
			order = append(order, p.Project)
		}
		by[p.Project] = append(by[p.Project], p.Path)
	}
	var left []byte
	for _, project := range order {
		if _, err := link.Sync(ctx, proto.RagSync{Project: project, Index: sessionsIndex, Gone: by[project]}); err != nil {
			for _, p := range by[project] {
				line, _ := json.Marshal(pendingForget{Project: project, Path: p})
				left = append(append(left, line...), '\n')
			}
		}
	}
	if len(left) == 0 {
		_ = os.Remove(file)
		return
	}
	_ = os.WriteFile(file, left, 0o600)
}

// withSessionSearch answers session_search (agent.SessionSearchTool's run).
func withSessionSearch(inner agent.ToolDispatcher, run func(context.Context, string) string,
	onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != agent.SessionSearchName {
			return inner(ctx, tc)
		}
		out := run(ctx, tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall(agent.SessionSearchName, a, out)
		}
		return out, nil
	}
}

// lateSessionIndex is the shaper's index: built with the harness, before
// its config is read through, so it asks for the session index each call.
type lateSessionIndex struct{ h *Harness }

// A fold is also a /journal entry (journal.go): a reference to its span's
// file when it was written, else the summary itself.
func (l lateSessionIndex) Put(ctx context.Context, d agent.SessionDoc) error {
	var err error
	path := ""
	if x := l.h.sessionIndex(); x != nil {
		if err = x.Put(ctx, d); err == nil {
			path = filepath.Join(x.dir, foldFileName(d.Key))
		}
	}
	if d.Kind == agent.SessionDocFold {
		l.h.journalFold(d.Name, d.Summary, path)
	}
	return err
}

func (l lateSessionIndex) Search(ctx context.Context, q string, n int) ([]agent.SessionHit, error) {
	if x := l.h.sessionIndex(); x != nil {
		return x.Search(ctx, q, n)
	}
	return nil, errors.New("no session index: this session has no dund link")
}

func (l lateSessionIndex) Close(context.Context) error { return nil }
