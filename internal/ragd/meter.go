package ragd

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// The rag meters are what `dun ps` shows of retrieval inside dund (USER
// 2026-11-17: "dun ps must cover rag activity"): ingest and embedding run in
// the dund process, so the process table shows them only as dund's CPU.
//
// One meter per open index (project and kind, a worktree branch its own).
// It counts what passes through dund's seams into raglit: the job's fetch
// (Worker.Fetcher, one per file a job takes), the index's embed client (one
// per embed request, its inputs the chunks), and the text ingests dund makes
// itself (ingestText). Totals are atomics; the last minute is a ring of
// buckets under the meter's own mutex, one add per event, no allocation.
// The queue and the newest failed job are read from the index's job table
// when dun ps asks (poll), not counted.

// The last-minute window: nBuckets buckets of bucketSec seconds.
const (
	bucketSec = 5
	nBuckets  = 12
)

// idleAfter: an index with no activity for this long is idle in dun ps, and
// a last error older than this no longer makes its state "error".
const idleAfter = 10 * time.Minute

// activeFor is how long after an event an index with nothing queued is still
// "ingesting" (a text ingest has no job row to see).
const activeFor = 5 * time.Second

// pollEvery bounds the job-table reads of one index: at most one per
// pollEvery, and for an idle index with nothing queued one per pollIdle.
const (
	pollEvery = 2 * time.Second
	pollIdle  = 60 * time.Second
)

// queueCap caps the pending jobs counted (the count is a page of the job
// table): a queue of more reads as queueCap.
const queueCap = 1000

// counts is one bucket, or a sum of them.
type counts struct {
	files, chunks, embeds, embedNs, tokens int64
}

func (c *counts) add(d counts) {
	c.files += d.files
	c.chunks += d.chunks
	c.embeds += d.embeds
	c.embedNs += d.embedNs
	c.tokens += d.tokens
}

// window is the last minute of counts: bucket i holds epoch[i], the
// bucketSec-long slot of time it counts.
type window struct {
	mu    sync.Mutex
	epoch [nBuckets]int64
	v     [nBuckets]counts
}

func (w *window) add(now time.Time, d counts) {
	e := now.Unix() / bucketSec
	i := e % nBuckets
	w.mu.Lock()
	if w.epoch[i] != e {
		w.epoch[i], w.v[i] = e, counts{}
	}
	w.v[i].add(d)
	w.mu.Unlock()
}

// sum is the buckets of the last nBuckets slots, the current one included.
func (w *window) sum(now time.Time) counts {
	e := now.Unix() / bucketSec
	var out counts
	w.mu.Lock()
	for i := range w.v {
		if age := e - w.epoch[i]; age >= 0 && age < nBuckets {
			out.add(w.v[i])
		}
	}
	w.mu.Unlock()
	return out
}

// meter is one index's counters.
type meter struct {
	name string
	now  func() time.Time

	win                                    window
	files, chunks, embeds, embedNs, tokens atomic.Int64 // totals since dund started
	inFlight                               atomic.Int64 // embed requests in flight
	lastActive, lastOK                     atomic.Int64 // unix nanoseconds; 0 never

	errMu     sync.Mutex
	lastErr   string
	lastErrAt time.Time

	// embedder is the index's embed client over the Host's (embedFor); set
	// under Host.mu.
	embedder *raglit.Embedder

	pollMu   sync.Mutex
	polledAt time.Time
	pending  int
	running  int
	jobErr   string // the newest failed job's error
	jobErrAt time.Time
	pollErr  string
}

func newMeter(name string, now func() time.Time) *meter {
	if now == nil {
		now = time.Now
	}
	return &meter{name: name, now: now}
}

func (m *meter) touch(t time.Time) { m.lastActive.Store(t.UnixNano()) }

func (m *meter) ok(t time.Time) {
	m.lastActive.Store(t.UnixNano())
	m.lastOK.Store(t.UnixNano())
}

// fail records err as the index's last error.
func (m *meter) fail(t time.Time, err error) {
	m.touch(t)
	msg := oneLine(err.Error())
	m.errMu.Lock()
	m.lastErr, m.lastErrAt = msg, t
	m.errMu.Unlock()
}

// fetched counts one file a job took (Worker.Fetcher).
func (m *meter) fetched(err error) {
	t := m.now()
	if err != nil {
		m.fail(t, err)
		return
	}
	m.files.Add(1)
	m.win.add(t, counts{files: 1})
	m.touch(t)
}

// ingested counts one text ingest (ingestText): a file of one chunk.
func (m *meter) ingested(err error) {
	t := m.now()
	if err != nil {
		m.fail(t, err)
		return
	}
	m.files.Add(1)
	m.chunks.Add(1)
	m.win.add(t, counts{files: 1, chunks: 1})
	m.ok(t)
}

// embedded counts one embed request of n inputs, chars long in all, that
// took d. Tokens are estimated as raglit's usage hook does: chars/4.
func (m *meter) embedded(d time.Duration, n, chars int, err error) {
	t := m.now()
	if err != nil {
		m.fail(t, err)
		return
	}
	c := counts{chunks: int64(n), embeds: 1, embedNs: int64(d), tokens: int64((chars + 3) / 4)}
	m.chunks.Add(c.chunks)
	m.embeds.Add(1)
	m.embedNs.Add(c.embedNs)
	m.tokens.Add(c.tokens)
	m.win.add(t, c)
	m.ok(t)
}

// poll reads the index's queue and newest failed job from st, at most once
// per pollEvery, and for an index with no recent activity and nothing queued
// once per pollIdle.
func (m *meter) poll(st *raglit.Store, now time.Time) (pending, running int, jobErr string, jobErrAt time.Time) {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	every := pollEvery
	if m.pending == 0 && m.running == 0 && now.Sub(unixNano(m.lastActive.Load())) > idleAfter {
		every = pollIdle
	}
	if m.polledAt.IsZero() || now.Sub(m.polledAt) >= every {
		m.polledAt = now
		m.pollErr = ""
		if js, err := st.Jobs("pending", queueCap); err == nil {
			m.pending = len(js)
		} else {
			m.pollErr = err.Error()
		}
		if js, err := st.Jobs("running", 100); err == nil {
			m.running = len(js)
		}
		if js, err := st.Jobs("error", 1); err == nil && len(js) == 1 {
			m.jobErr, m.jobErrAt = oneLine(js[0].Error), unixNano(js[0].FinishedAt)
		}
	}
	return m.pending, m.running, m.jobErr, m.jobErrAt
}

// snapshot is the meter as dun ps shows it. chilled: the embed model's
// admission channel is cooling down after a 429.
func (m *meter) snapshot(now time.Time, pending, running int, jobErr string, jobErrAt time.Time, chilled bool) proto.ProcRagIndex {
	w := m.win.sum(now)
	out := proto.ProcRagIndex{Name: m.name, Queue: pending, Running: running,
		FilesMin: w.files, ChunksMin: w.chunks, EmbedsMin: w.embeds, EmbedMsMin: w.embedNs / 1e6, TokensMin: w.tokens,
		Files: m.files.Load(), Chunks: m.chunks.Load(), Embeds: m.embeds.Load(),
		EmbedMs: m.embedNs.Load() / 1e6, Tokens: m.tokens.Load(), EmbedInFlight: int(m.inFlight.Load()),
		IdleSec: -1}
	out.Project, out.Kind, out.Branch = splitName(m.name)
	if pending >= queueCap {
		out.QueueCapped = true
	}
	m.errMu.Lock()
	out.LastErr, out.LastErrAt = m.lastErr, m.lastErrAt
	m.errMu.Unlock()
	if jobErr != "" && jobErrAt.After(out.LastErrAt) {
		out.LastErr, out.LastErrAt = jobErr, jobErrAt
	}
	last := unixNano(m.lastActive.Load())
	if out.LastErrAt.After(last) {
		last = out.LastErrAt
	}
	if !last.IsZero() {
		out.LastActive = last
		out.IdleSec = int64(now.Sub(last).Seconds())
	}
	out.State = state(now, out, unixNano(m.lastOK.Load()), chilled)
	return out
}

// Index states, as dun ps shows them.
const (
	StateIdle      = "idle"
	StateIngesting = "ingesting"
	StateEmbedding = "embedding"
	StateBackedOff = "backed-off"
	StateError     = "error"
)

// state is an index's state: backed-off while it has work and the embed
// model's channel cools down after a 429; embedding while an embed request
// of it is in flight; ingesting while it has jobs queued or running, or had
// an event in the last activeFor; error when its last error is newer than
// its last success and less than idleAfter old; else idle.
func state(now time.Time, ix proto.ProcRagIndex, lastOK time.Time, chilled bool) string {
	busy := ix.Queue > 0 || ix.Running > 0 || ix.EmbedInFlight > 0
	switch {
	case busy && chilled:
		return StateBackedOff
	case ix.EmbedInFlight > 0:
		return StateEmbedding
	case ix.Queue > 0 || ix.Running > 0:
		return StateIngesting
	case !ix.LastErrAt.IsZero() && ix.LastErrAt.After(lastOK) && now.Sub(ix.LastErrAt) < idleAfter:
		return StateError
	case !ix.LastActive.IsZero() && now.Sub(ix.LastActive) < activeFor:
		return StateIngesting
	}
	return StateIdle
}

// splitName is an index key's project ("" global), kind and worktree
// branch ("" the main checkout): "dun/code", "global/memories",
// "dun/branches/wt-1a2b3c4d/code".
func splitName(name string) (project, kind, branch string) {
	parts := strings.Split(name, "/")
	project, kind = parts[0], parts[len(parts)-1]
	if project == Global {
		project = ""
	}
	if len(parts) == 4 && parts[1] == branchesDir {
		branch = parts[2]
	}
	return project, kind, branch
}

func unixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func oneLine(s string) string {
	return textfmt.ClipRune(textfmt.OneLine(s), 199)
}

// meteredVector is an index's embed client: the Host's (embedRoute), with
// each request counted on the index's meter and in the Host's in-flight
// count. A search's query embedding is counted in flight only: it is not
// ingest.
type meteredVector struct {
	inner    raglit.VectorClient
	m        *meter
	inFlight *atomic.Int64 // the Host's
}

func (v meteredVector) Embed(ctx context.Context, model string, input []string) ([][]float32, error) {
	v.inFlight.Add(1)
	defer v.inFlight.Add(-1)
	if ctx.Value(searchKey{}) != nil {
		return v.inner.Embed(ctx, model, input)
	}
	v.m.inFlight.Add(1)
	start := time.Now()
	out, err := v.inner.Embed(ctx, model, input)
	v.m.inFlight.Add(-1)
	chars := 0
	for _, s := range input {
		chars += len(s)
	}
	if err != nil && ctx.Err() != nil {
		// Cut off by a stop: not the index's error.
		return out, err
	}
	v.m.embedded(time.Since(start), len(input), chars, err)
	return out, err
}

// meterLocked is name's meter (made on first use), for store st, under h.mu.
func (h *Host) meterLocked(name string, st *raglit.Store) *meter {
	m := h.meters[name]
	if m == nil {
		m = newMeter(name, h.now)
		h.meters[name] = m
	}
	if st != nil {
		h.meterOf[st] = m
	}
	return m
}

// meterFor is st's meter, nil when st is not an index of h.
func (h *Host) meterFor(st *raglit.Store) *meter {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.meterOf[st]
}

// touch marks st active now (a job queued, a document removed).
func (h *Host) touch(st *raglit.Store) {
	if m := h.meterFor(st); m != nil {
		m.touch(m.now())
	}
}

// embedFor is name's embed client, under h.mu: the Host's embedder over a
// meteredVector, one per index and kept, as raglit compares an index's
// embedder by identity.
func (h *Host) embedFor(name string) *raglit.Embedder {
	if h.embed == nil {
		return nil
	}
	m := h.meterLocked(name, nil)
	if m.embedder == nil {
		e := *h.embed
		e.Client = meteredVector{inner: h.embed.Client, m: m, inFlight: &h.embedInFlight}
		m.embedder = &e
	}
	return m.embedder
}

// meteredWorker is newWorker for st with its fetch counted on st's meter.
func (h *Host) meteredWorker(st *raglit.Store, models raglit.Models) *raglit.Worker {
	w := raglit.NewWorker(st, models, nil)
	if m := h.meterFor(st); m != nil {
		next := w.Fetcher
		w.Fetcher = func(ctx context.Context, u string) (raglit.Fetched, error) {
			var f raglit.Fetched
			var err error
			if next != nil {
				f, err = next(ctx, u)
			} else {
				f, err = raglit.Fetch(ctx, u)
			}
			if err != nil && ctx.Err() != nil {
				return f, err
			}
			m.fetched(err)
			return f, err
		}
	}
	return w
}

// Status is retrieval's activity for dun ps (proto.Procs): the embed
// backend, and each open index's state, queue and counters.
func (h *Host) Status() *proto.ProcRag {
	now := h.now()
	out := &proto.ProcRag{Indexes: []proto.ProcRagIndex{}}
	chilled := false
	if h.embed != nil {
		out.Embed = proto.ProcRagEmbed{Model: h.embed.Model, Where: where(h.models.Embed.BaseURL)}
		if h.models.Channels != nil {
			for _, c := range h.models.Channels.Stats() {
				if c.Model == h.embed.Model {
					out.Embed.Width, out.Embed.N429, out.Embed.BackedOff = c.Width, c.N429, c.Chilled
					chilled = c.Chilled
				}
			}
		}
	}
	out.Embed.InFlight = int(h.embedInFlight.Load())
	type open struct {
		m  *meter
		st *raglit.Store
	}
	h.mu.Lock()
	ixs := make([]open, 0, len(h.stores))
	for name, st := range h.stores {
		ixs = append(ixs, open{h.meterLocked(name, st), st})
	}
	h.mu.Unlock()
	for _, ix := range ixs {
		p, r, je, jeAt := ix.m.poll(ix.st, now)
		out.Indexes = append(out.Indexes, ix.m.snapshot(now, p, r, je, jeAt, chilled))
	}
	sort.Slice(out.Indexes, func(i, j int) bool { return out.Indexes[i].Name < out.Indexes[j].Name })
	return out
}

// where is "local" for an embed endpoint on this machine or the local
// network (a loopback or private address, localhost, a .local name),
// "remote" for any other, "" with no URL.
func where(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "remote"
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") {
		return "local"
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return "local"
	}
	return "remote"
}
