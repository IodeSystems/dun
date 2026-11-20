package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Turn errors, kept for diagnosis (aw4 i_1m7rqbptp6z6f22q6jgff8: real logs of
// "agent: chat: JSON error injected into SSE stream" from one endpoint).
// The transcript already keeps the error's text (kindTurnError, turnerror.go,
// on the engine's path only); that says nothing of where it came from, and
// reading it back means reading every transcript.
//
// A sidecar, <session>.errors.jsonl, beside the transcript: a structured
// record per failed turn, for every turn path (engine, -p, children: all run
// turnThenTriggers), bounded. removeSidecars deletes it with its session.
// `dun errors` reads it on any machine (cmd/dun/errorscmd.go).
//
// What agentkit v0.38.0 hands dun: an SSE error event as *llm.StreamError
// (code, type, param, metadata, the raw object); a silent link as
// *llm.StallError (phase, limit); a round with no text and no tool call,
// after agentkit's one repair round, as *agent.EmptyReplyError (the
// provider's finish_reason and native_finish_reason). A refused HTTP request
// keeps its status and body only on the typed errors (InputTooLargeError,
// BudgetSpentError, TruncatedToolCallError); the others carry the status in
// their text, "llm: status 502: …", which is parsed.

// TurnError is one turn that ended in an error.
type TurnError struct {
	Time     time.Time `json:"time"`
	Session  string    `json:"session"`
	Provider string    `json:"provider,omitempty"`
	Host     string    `json:"host,omitempty"` // the endpoint URL's host
	Model    string    `json:"model,omitempty"`
	Error    string    `json:"error"`
	Status   int       `json:"status,omitempty"` // HTTP status, when there was one
	// Payload is the server's own body as agentkit kept it, or empty when
	// agentkit kept none beyond the error text.
	Payload string `json:"payload,omitempty"`
	// Code, Type and Metadata are an SSE error event's error.code, error.type
	// and error.metadata (OpenRouter: provider_name, the upstream's raw body).
	Code     string `json:"code,omitempty"`
	Type     string `json:"type,omitempty"`
	Metadata string `json:"metadata,omitempty"`
	// Finish and NativeFinish are an empty reply's finish_reason and
	// native_finish_reason, verbatim.
	Finish       string `json:"finish,omitempty"`
	NativeFinish string `json:"native_finish,omitempty"`
	// Stall is a stalled request's phase: first_byte or between_chunks.
	Stall string `json:"stall,omitempty"`
	// Trace is the id of the session's last request before the error
	// (trace.go), as sent in its X-Trace-Id header.
	Trace string `json:"trace,omitempty"`
}

const (
	turnErrorPayloadMax = 8 << 10 // bytes of payload (and of error text) kept
	turnErrorsKept      = 200     // records kept per session file, newest
)

// TurnErrorsFile is the sidecar a session's turn errors are kept in.
func TurnErrorsFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".errors.jsonl"
}

// noteTurnError keeps err, the error a turn ended with, in the session's
// errors file. A turn the caller canceled (an interrupt, a closed session) or
// held is not an error of the provider's and is not kept; nor is anything for
// an in-memory session.
func (h *Harness) noteTurnError(ctx context.Context, err error) {
	if err == nil || h.cfg.SessionFile == "" || ctx.Err() != nil || errors.Is(err, ErrTurnHeld) {
		return
	}
	rec := turnErrorRecord(err, h.cfg.Provider, h.cfg.Endpoint, time.Now())
	rec.Session = strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), ".jsonl")
	rec.Trace = h.trace.Last()
	if e := appendTurnError(TurnErrorsFile(h.cfg.SessionFile), rec); e != nil {
		log.Printf("dun: keep turn error: %v", e)
	}
}

// shownError is a turn error with dun's own text for the person: the text
// the UI shows and errors.jsonl keeps. Unwrap keeps errors.As working.
type shownError struct {
	msg string
	err error
}

func (e *shownError) Error() string { return e.msg }
func (e *shownError) Unwrap() error { return e.err }

// describeTurnError gives the errors a person cannot read from agentkit's
// text dun's own: an empty reply names the model and the finish reasons, a
// stall says how long the link was silent. Others are err unchanged.
//
// The empty reply was dun's to detect before agentkit v0.38.0 (Reply ""
// and no error; Gemini 4, 2026-11-19). agentkit now detects it, runs one
// repair round, and returns *agent.EmptyReplyError; this is the notice
// (docs/decisions.md "agentkit owns the empty reply").
func (h *Harness) describeTurnError(err error) error {
	var empty *agent.EmptyReplyError
	var stall *llm.StallError
	switch {
	case errors.As(err, &empty):
		model := h.cfg.Endpoint.Model
		if model == "" {
			model = "the model"
		}
		return &shownError{emptyReplyText(model, empty), err}
	case errors.As(err, &stall):
		return &shownError{stallText(stall), err}
	}
	return err
}

// emptyReplyText is the notice for a turn that ended with no reply.
func emptyReplyText(model string, e *agent.EmptyReplyError) string {
	s := model + " returned an empty reply: the turn ended with no text from the model"
	if e.FinishReason == "" && e.NativeFinishReason == "" {
		return s + " (no finish_reason)"
	}
	s += " (finish_reason " + orNone(e.FinishReason)
	if e.NativeFinishReason != "" {
		s += ", native " + e.NativeFinishReason
	}
	return s + ")"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// stallText is "stalled: no bytes for 2m (between_chunks)".
func stallText(e *llm.StallError) string {
	return fmt.Sprintf("stalled: no bytes for %s (%s)", shortDur(e.After), e.Phase)
}

// shortDur is d without zero tails: 2m, 10m, 1m30s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

var statusInText = regexp.MustCompile(`\bstatus (\d{3})\b`)

// turnErrorRecord is err as a record: scrubbed of the endpoint's key and of
// anything shaped like a credential, and capped.
func turnErrorRecord(err error, provider string, ep Endpoint, at time.Time) TurnError {
	rec := TurnError{Time: at.UTC(), Provider: provider, Model: ep.Model, Error: err.Error()}
	if u, e := url.Parse(ep.URL); e == nil {
		rec.Host = u.Host
	}
	var tooLarge *llm.InputTooLargeError
	var spent *llm.BudgetSpentError
	var cut *llm.TruncatedToolCallError
	var se *llm.StreamError
	var empty *agent.EmptyReplyError
	var stall *llm.StallError
	switch {
	case errors.As(err, &tooLarge):
		rec.Status, rec.Payload = tooLarge.Status, tooLarge.Body
	case errors.As(err, &spent):
		rec.Status, rec.Payload = spent.Status, spent.Body
	case errors.As(err, &cut):
		rec.Status, rec.Payload = cut.Status, cut.Body
	case errors.As(err, &se):
		rec.Code, rec.Type = se.Code, se.Type
		rec.Metadata = capBytes(scrubSecrets(string(se.Metadata), ep.Key), turnErrorPayloadMax)
	case errors.As(err, &empty):
		rec.Finish, rec.NativeFinish = empty.FinishReason, empty.NativeFinishReason
	case errors.As(err, &stall):
		rec.Stall = stall.Phase
	}
	if rec.Status == 0 {
		if m := statusInText.FindStringSubmatch(rec.Error); m != nil {
			rec.Status, _ = strconv.Atoi(m[1])
		}
	}
	rec.Error = capBytes(scrubSecrets(rec.Error, ep.Key), turnErrorPayloadMax)
	rec.Payload = capBytes(scrubSecrets(rec.Payload, ep.Key), turnErrorPayloadMax)
	return rec
}

// secretShapes are credentials as they appear in a header dump, a JSON body
// or an error string: an Authorization or key header or field, a bearer
// token, an sk- key.
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)("?(?:authorization|proxy-authorization|x-api-key|api[-_]?key|access[-_]?token|x-goog-api-key)"?\s*[:=]\s*)("[^"]*"|'[^']*'|(?:bearer|basic)\s+\S+|[^\s,;}&]+)`),
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
}

// scrubSecrets replaces key (the endpoint's own) and anything shaped like a
// credential with [redacted].
func scrubSecrets(s, key string) string {
	if s == "" {
		return s
	}
	if len(key) >= 4 {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	s = secretShapes[0].ReplaceAllString(s, "${1}[redacted]")
	s = secretShapes[1].ReplaceAllString(s, "$1 [redacted]")
	return secretShapes[2].ReplaceAllString(s, "[redacted]")
}

// capBytes is s cut to at most n bytes on a rune boundary, saying how much
// was cut.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[%d bytes cut]", len(s)-cut)
}

var turnErrorsMu sync.Mutex

// appendTurnError adds rec to path and keeps the newest turnErrorsKept.
// Errors are rare, so the file is read and rewritten whole (at most 200
// records of at most ~16 KiB).
func appendTurnError(path string, rec TurnError) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	turnErrorsMu.Lock()
	defer turnErrorsMu.Unlock()
	var lines [][]byte
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(l)) > 0 {
				lines = append(lines, l)
			}
		}
	}
	lines = append(lines, line)
	if len(lines) > turnErrorsKept {
		lines = lines[len(lines)-turnErrorsKept:]
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadTurnErrors is the records in one errors file, oldest first; a line
// that does not parse is skipped.
func ReadTurnErrors(path string) ([]TurnError, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []TurnError
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r TurnError
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Error != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// AllTurnErrors is every session's records under $DUN_HOME/sessions, or
// session's alone (and its children's) when it is not "", oldest first.
func AllTurnErrors(session string) ([]TurnError, error) {
	pat := "*.errors.jsonl"
	if session != "" {
		pat = session + ".*errors.jsonl"
	}
	files, err := filepath.Glob(filepath.Join(SessionsDir(), "*", pat))
	if err != nil {
		return nil, err
	}
	var out []TurnError
	for _, f := range files {
		recs, err := ReadTurnErrors(f)
		if err != nil {
			continue // removed under us, or unreadable: the rest still count
		}
		out = append(out, recs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}
