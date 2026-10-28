package dun

import (
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Request stats: how fast the last model request went, for a status bar
// (owner, 2026-10-27: "quick stats when there's room on the topbar … cache,
// ttft, tps, tg (per request) … realtime ctx len").
//
// WHICH requests: the session's own turn rounds — the ones that go through
// the turn's tee (reasoning.go). Side calls (suggest, rephrase, commit, the
// memory fork) untee first, and a sub-agent's harness has no OnRequest, so
// neither is counted. A bar that mixed a 200-token suggestion into the main
// model's speed would show neither.
//
// WHAT is measured, and where:
//   - ttft: client side, from the request being handed to the client to the
//     first streamed content or reasoning delta. It includes the network and
//     any queueing at the proxy: it is what the person waits.
//   - pp (prompt processing, tokens a second): llama.cpp's own prompt_n /
//     prompt_ms when the server sends timings; else the uncached prompt
//     tokens over ttft, which counts the wait as processing and so reads low.
//   - tg (generation, tokens a second): llama.cpp's predicted_n /
//     predicted_ms when sent; else the completion tokens over the time from
//     the first token to the last. LIVE while the reply streams: the streamed
//     characters at the measured chars-per-token (tokencal.go), over the time
//     since the first token, at most reqLiveEvery apart; the final figure
//     replaces it.
//   - cache: the request's cached prompt tokens over its prompt tokens; only
//     when the provider reported the cache field (CacheReported).
//   - ctx: what the conversation fills of the window. At the start, the
//     engine's estimate of the prompt just built, at least the measured
//     prefix (system prompt and tool schemas); while streaming, plus the
//     tokens generated so far; at the end, the provider's prompt tokens plus
//     its completion tokens (the reply is context for the next round). A
//     recap or compaction moves it at once by what it removed (adjustCtx).

// RequestStat is one request's figures at one moment. Rates are 0 when not
// measured yet; Exact says the generation figures are the provider's count
// rather than the live estimate.
type RequestStat struct {
	Phase string // "start", "live", "done", or "ctx" (a recap or compaction moved the context)

	TTFTMS int64

	Prompt        int  // the provider's prompt tokens; 0 until reported
	Cached        int  // of them, reused from its cache
	CacheReported bool // the provider reported the cache field on this request

	PPS       float64 // prompt tokens a second
	PPSServer bool    // PPS is the server's own timing

	Gen       int     // tokens generated (estimated while live)
	TGS       float64 // generated tokens a second
	TGSServer bool    // TGS is the server's own timing
	Exact     bool    // Gen is the provider's count

	Ctx    int // the context in use, in tokens
	Window int // the whole window; 0 when unknown
}

// reqLiveEvery is the live update's minimum gap: about 4 a second, which a
// person reads as moving and which costs dund one small event per 250ms of
// streaming — against one event per token already.
const reqLiveEvery = 250 * time.Millisecond

// reqMeter measures the turn's requests. One request at a time: the turn's
// rounds are sequential.
type reqMeter struct {
	mu sync.Mutex
	on func(RequestStat)
	// cpt is the chars-per-token for the live estimate, window the window,
	// est the estimate of the prompt just built, and floor the prefix
	// (system prompt and tool schemas) as last measured.
	cpt    func() float64
	window func() int
	est    func() int
	floor  func() int
	now    func() time.Time

	cur      RequestStat
	sent     time.Time
	first    time.Time
	last     time.Time
	lastEmit time.Time
	chars    int
	base     int // ctx before the reply: the prompt estimate, then the report
	usage    *llm.Usage
	open     bool
}

// begin opens a request, at the moment it is handed to the client.
func (m *reqMeter) begin() {
	if m == nil || m.on == nil {
		return
	}
	m.mu.Lock()
	now := m.now()
	m.sent, m.first, m.last, m.lastEmit, m.chars, m.usage, m.open = now, time.Time{}, time.Time{}, time.Time{}, 0, nil, true
	// The prefix is the floor: before the ratio is measured, the build's
	// estimate leaves out the tool schemas (live, 2026-10-27: 468 estimated,
	// 5.7k reported on the first request; 5.5k against 5.9k once measured).
	// Correcting by the last request's error does not work: the estimate
	// is recalibrated between the two, and the correction counts it twice.
	m.base = m.est()
	if m.floor != nil {
		m.base = max(m.base, m.floor())
	}
	m.cur = RequestStat{Phase: "start", Ctx: m.base, Window: m.window()}
	ev := m.cur
	m.mu.Unlock()
	m.on(ev)
}

// chunk notes one streamed chunk; it emits the first token at once and live
// figures at most reqLiveEvery apart.
func (m *reqMeter) chunk(c llm.StreamChunk) {
	if m == nil || m.on == nil {
		return
	}
	m.mu.Lock()
	if !m.open {
		m.mu.Unlock()
		return
	}
	if c.Usage != nil {
		m.usage = c.Usage
	}
	n := len(c.Content) + len(c.Reasoning)
	if n == 0 {
		m.mu.Unlock()
		return
	}
	now := m.now()
	firstNow := m.first.IsZero()
	if firstNow {
		m.first = now
		m.cur.TTFTMS = now.Sub(m.sent).Milliseconds()
	}
	m.last = now
	m.chars += n
	if !firstNow && now.Sub(m.lastEmit) < reqLiveEvery {
		m.mu.Unlock()
		return
	}
	m.lastEmit = now
	m.cur.Phase = "live"
	cpt := m.cpt()
	if cpt <= 0 {
		cpt = fallbackCharsPerToken
	}
	m.cur.Gen = int(float64(m.chars)/cpt + 0.5)
	if dt := now.Sub(m.first).Seconds(); dt > 0 {
		m.cur.TGS = float64(m.cur.Gen) / dt
	}
	m.cur.Ctx = m.base + m.cur.Gen
	ev := m.cur
	m.mu.Unlock()
	m.on(ev)
}

// end closes the request with the provider's figures, when it sent any.
func (m *reqMeter) end() {
	if m == nil || m.on == nil {
		return
	}
	m.mu.Lock()
	if !m.open {
		m.mu.Unlock()
		return
	}
	m.open = false
	r := m.cur
	r.Phase = "done"
	r.Window = m.window()
	if u := m.usage; u != nil {
		r.Prompt = u.PromptTokens
		r.Cached = u.CachedPromptTokens()
		r.CacheReported = u.PromptTokensDetails != nil || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
		if u.CompletionTokens > 0 {
			r.Gen, r.Exact = u.CompletionTokens, true
		}
		r.PPS, r.PPSServer, r.TGS, r.TGSServer = 0, false, 0, false
		if t := u.Timings; t != nil && t.PromptMS > 0 {
			r.PPS, r.PPSServer = float64(t.PromptN)/(t.PromptMS/1000), true
		} else if uncached := r.Prompt - r.Cached; uncached > 0 && r.TTFTMS > 0 {
			r.PPS = float64(uncached) / (float64(r.TTFTMS) / 1000)
		}
		if t := u.Timings; t != nil && t.PredictedMS > 0 {
			r.TGS, r.TGSServer = float64(t.PredictedN)/(t.PredictedMS/1000), true
		} else if dt := m.last.Sub(m.first).Seconds(); dt > 0 && r.Gen > 0 {
			r.TGS = float64(r.Gen) / dt
		}
		if r.Prompt > 0 {
			m.base = r.Prompt
		}
	}
	r.Ctx = m.base + r.Gen
	m.cur = r
	m.mu.Unlock()
	m.on(r)
}

// adjustCtx moves the context figure by delta tokens (negative: a recap or
// compaction removed that much) and says so at once.
func (m *reqMeter) adjustCtx(delta int) {
	if m == nil || m.on == nil || delta == 0 {
		return
	}
	m.mu.Lock()
	if m.sent.IsZero() { // no request yet: no figure to move
		m.mu.Unlock()
		return
	}
	m.cur.Ctx = max(0, m.cur.Ctx+delta)
	m.base = max(0, m.base+delta)
	r := m.cur
	r.Phase, r.Window = "ctx", m.window()
	m.mu.Unlock()
	m.on(r)
}

// newReqMeter is h's meter, wired to its OnRequest and its estimator; nil
// when nothing listens.
func (h *Harness) newReqMeter() *reqMeter {
	if h.cfg.OnRequest == nil {
		return nil
	}
	return &reqMeter{
		on:     h.cfg.OnRequest,
		cpt:    h.meter.CharsPerToken,
		window: h.windowTokens,
		est:    h.meter.lastPromptTokens,
		floor:  func() int { return h.SystemParts().Total },
		now:    time.Now,
	}
}
