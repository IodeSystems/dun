package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Trace ids: every model request the harness sends carries one, so a line in
// a provider's or gateway's log leads back to the session, the role and the
// dun log line (Carl, 2026-11-20: a request whose input was "word word word…"
// was raglit's context probe, and nothing said so).
//
//	dun-<session>-<run>-<role>-<seq>     e.g. dun-20261120-150405-7f3a-main-12
//
// <session> is the session file's id (a start time, no name or path); <run>
// is four random hex digits per engine start, so a resumed session does not
// reuse the ids of its earlier run; <role> is main, child#N (a sub-agent's
// rounds), or a side call's kind (fold, rescue, suggest, …), prefixed with
// child#N. for a sub-agent's; <seq> counts the session tree's requests. The
// id goes to external providers: it holds nothing from the prompt.
//
// It is sent as ChatOpts.TraceID, which agentkit v0.39.0 puts in the
// X-Trace-Id header of the request. The provider's own request id
// (llm.Usage.RequestID) is logged next to it.

// traceHub is what a session tree shares: the request counter and the
// rounds waiting for their trace id.
type traceHub struct {
	seq    atomic.Int64
	mu     sync.Mutex
	open   int // traced streams in flight
	parked []*parkedRound
}

// tracer names one harness's requests.
type tracer struct {
	hub    *traceHub
	prefix string // dun-<session>-<run>
	scope  string // "" for a root session, child#N for a sub-agent
	last   atomic.Value
}

// newTracer is a root session's tracer. session is the session's id.
func newTracer(session string) *tracer {
	if session == "" {
		session = "mem"
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	return &tracer{hub: &traceHub{}, prefix: "dun-" + session + "-" + hex.EncodeToString(b[:])}
}

// child is sub-agent num's tracer: the parent's prefix and counter.
func (t *tracer) child(num int) *tracer {
	if t == nil {
		return nil
	}
	scope := fmt.Sprintf("child#%d", num)
	if t.scope != "" {
		scope = fmt.Sprintf("%s.%d", t.scope, num)
	}
	return &tracer{hub: t.hub, prefix: t.prefix, scope: scope}
}

// mainRole is the role of this session's own rounds.
func (t *tracer) mainRole() string {
	if t.scope != "" {
		return t.scope
	}
	return "main"
}

// role is a side call's role in this session.
func (t *tracer) role(kind string) string {
	if t.scope != "" {
		return t.scope + "." + kind
	}
	return kind
}

// next is a new id for role.
func (t *tracer) next(role string) string {
	return fmt.Sprintf("%s-%s-%d", t.prefix, role, t.hub.seq.Add(1))
}

// Last is the id of this session's latest own round, "" before one.
func (t *tracer) Last() string {
	if t == nil {
		return ""
	}
	s, _ := t.last.Load().(string)
	return s
}

// stamp gives opts a trace id for role, copying them; opts that already
// carry one (an outer traced runner named the call) come back unchanged,
// with ok false.
func (t *tracer) stamp(role string, opts *llm.ChatOpts) (out *llm.ChatOpts, id string, ok bool) {
	if t == nil {
		return opts, "", false
	}
	if opts != nil && opts.TraceID != "" {
		return opts, opts.TraceID, false
	}
	o := llm.ChatOpts{}
	if opts != nil {
		o = *opts
	}
	o.TraceID = t.next(role)
	return &o, o.TraceID, true
}

// traceCall is one traced request in flight.
type traceCall struct {
	t        *tracer
	id, role string
	u        *llm.Usage
	errText  string
	closed   bool // close ran; one goroutine calls see and close
}

// open stamps opts with a new id for role and counts the request in flight.
// A nil call (opts already named, or no tracer) needs no see or close.
func (t *tracer) open(role string, opts *llm.ChatOpts) (*llm.ChatOpts, *traceCall) {
	sent, id, fresh := t.stamp(role, opts)
	if !fresh {
		return sent, nil
	}
	if role == t.mainRole() {
		t.last.Store(id)
	}
	t.hub.begin()
	return sent, &traceCall{t: t, id: id, role: role}
}

// see notes one chunk of the reply.
func (c *traceCall) see(ch llm.StreamChunk) {
	if c == nil {
		return
	}
	if ch.Usage != nil {
		c.u = ch.Usage
	}
	if ch.Error != "" {
		c.errText = ch.Error
	}
}

// failed ends a request that got no stream.
func (c *traceCall) failed(err error) {
	if c == nil {
		return
	}
	c.t.hub.end()
	log.Printf("dun: trace %s %s: %v", c.id, c.role, err)
}

// close ends the request once its stream closed: agentkit calls OnUsage
// before it closes the stream, so the round this usage made is parked by
// now (noteRound) and is claimed here. One log line per request.
func (c *traceCall) close() {
	if c == nil || c.closed {
		return
	}
	c.closed = true
	c.t.hub.end()
	if c.u == nil {
		log.Printf("dun: trace %s %s: no usage%s", c.id, c.role, errSuffix(c.errText))
		return
	}
	u := c.u
	c.t.hub.claim(roundKeyOf(*u), c.id)
	log.Printf("dun: trace %s %s: finish=%s prompt=%d completion=%d %dms provider_id=%s%s",
		c.id, c.role, orDash(u.FinishReason), u.PromptTokens, u.CompletionTokens, u.LatencyMS, orDash(u.RequestID), errSuffix(c.errText))
}

// send runs one traced request: stamps opts, sends, and watches the reply.
func (t *tracer) send(ctx context.Context, role string, r agent.LLMRunner, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	sent, call := t.open(role, opts)
	in, err := r.ChatStream(ctx, msgs, tools, sent)
	if call == nil {
		return in, err
	}
	if err != nil || in == nil {
		call.failed(err)
		return in, err
	}
	out := make(chan llm.StreamChunk, cap(in)+1)
	go func() {
		defer close(out)
		defer call.close()
		for c := range in {
			call.see(c)
			out <- c
		}
	}()
	return out, nil
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " error=" + oneLine(s, 200)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// tracedRunner is a side call's runner: each request gets a trace id for
// its role. No other method of the runner under it is reached through it,
// so it wraps only runners used for ChatStream (sideRunner's).
type tracedRunner struct {
	inner agent.LLMRunner
	t     *tracer
	role  string
}

func (r tracedRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	return r.t.send(ctx, r.role, r.inner, msgs, tools, opts)
}

// traced is r with each request traced as side call kind.
func (h *Harness) traced(kind string, r agent.LLMRunner) agent.LLMRunner {
	if h.trace == nil || r == nil {
		return r
	}
	return tracedRunner{inner: r, t: h.trace, role: h.trace.role(kind)}
}

// Rounds and trace ids meet here. A round's stats arrive through the spend
// meter (OnUsage, no context); its trace id is known to the stream that
// carried it. agentkit calls OnUsage before the stream closes, so while a
// traced stream is open a round is parked, and the stream claims it by its
// usage at close. A round nobody claims is written without an id after
// parkFor. Two concurrent rounds with the same latency, token counts, finish
// and provider id would swap ids; the provider id makes that unlikely.
// agentkit echoing the trace id on llm.Usage would replace this (request
// filed with agentkit's owner, 2026-11-20).

type roundKey struct {
	latency            int64
	prompt, completion int
	finish, reqID      string
}

func roundKeyOf(u llm.Usage) roundKey {
	return roundKey{u.LatencyMS, u.PromptTokens, u.CompletionTokens, u.FinishReason, u.RequestID}
}

type parkedRound struct {
	key   roundKey
	r     RoundStat
	write func(RoundStat)
	done  bool
}

const parkFor = 2 * time.Second

func (hub *traceHub) begin() {
	hub.mu.Lock()
	hub.open++
	hub.mu.Unlock()
}

func (hub *traceHub) end() {
	hub.mu.Lock()
	hub.open--
	hub.mu.Unlock()
}

// park holds r for a traced stream to claim; false when none is open, and
// the caller writes r itself.
func (hub *traceHub) park(k roundKey, r RoundStat, write func(RoundStat)) bool {
	if hub == nil {
		return false
	}
	hub.mu.Lock()
	if hub.open == 0 {
		hub.mu.Unlock()
		return false
	}
	p := &parkedRound{key: k, r: r, write: write}
	hub.parked = append(hub.parked, p)
	hub.mu.Unlock()
	time.AfterFunc(parkFor, func() {
		if hub.take(p) {
			p.write(p.r)
		}
	})
	return true
}

// take removes p; false when it was already claimed.
func (hub *traceHub) take(p *parkedRound) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if p.done {
		return false
	}
	p.done = true
	for i, q := range hub.parked {
		if q == p {
			hub.parked = append(hub.parked[:i], hub.parked[i+1:]...)
			break
		}
	}
	return true
}

// claim writes the parked round with key k under id.
func (hub *traceHub) claim(k roundKey, id string) {
	hub.mu.Lock()
	var p *parkedRound
	for _, q := range hub.parked {
		if q.key == k {
			p = q
			break
		}
	}
	hub.mu.Unlock()
	if p != nil && hub.take(p) {
		p.r.Trace = id
		p.write(p.r)
	}
}

// sessionOfFile is the session id a session file names, sub-agent suffixes
// cut: "<id>.sub2.jsonl" → "<id>".
func sessionOfFile(file string) string {
	base := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base
}

// TraceID is a dun or dund trace id's parts.
type TraceID struct {
	Session, Run, Role string
	Seq                string
}

var traceRe = regexp.MustCompile(`^dun-([0-9]{8}-[0-9]{6}(?:-[0-9]+)?|mem)-([0-9a-f]{4})-(.+)-([0-9]+)$`)

// ParseTraceID splits a dun trace id; ok is false for anything else.
func ParseTraceID(id string) (TraceID, bool) {
	m := traceRe.FindStringSubmatch(id)
	if m == nil {
		return TraceID{}, false
	}
	return TraceID{Session: m[1], Run: m[2], Role: m[3], Seq: m[4]}, true
}

// SessionTag marks every request c sends as session's: an X-Session-Id
// header "dun-<session>" on every endpoint, and the OpenAI "user" field with
// the same value only where the endpoint at url is known to accept it
// (AcceptsUserField): a strict server may refuse a body field it does not
// know, while an unknown header is ignored. OpenRouter keeps them on the
// generation as session_id and external_user. The per-request id is the
// trace id (X-Trace-Id).
func SessionTag(c *llm.Client, session, url, dialect string) {
	if c == nil || session == "" {
		return
	}
	tag := "dun-" + session
	if AcceptsUserField(url, dialect) {
		c.User = tag
	}
	if c.Header == nil {
		c.Header = http.Header{}
	}
	c.Header.Set("X-Session-Id", tag)
}

// AcceptsUserField reports whether the endpoint at url, with its configured
// reasoning dialect ("" unknown), takes the "user" body field: OpenRouter
// (its host or its dialect) and api.openai.com.
func AcceptsUserField(url, dialect string) bool {
	if dialect == llm.DialectOpenRouter {
		return true
	}
	u, err := neturl.Parse(url)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai") || host == "api.openai.com"
}
