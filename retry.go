package dun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// RetryNote is one report about waiting on the provider, for a UI to render.
//
// dun retries at TWO levels and a user cannot be expected to care which:
//
//   - request scope — inside llm.Client, before any token is produced: a 429, a
//     5xx, a gateway that is not answering. The client already handled these
//     correctly and silently; this carries its narration out.
//   - turn scope — dun re-running the whole turn, for the faults the client
//     cannot retry: a stream that DIED MID-GENERATION (tokens were already
//     emitted, so the request is not resumable) or a client-level budget that ran
//     out. The turn resumes from persisted state, so completed tool calls are not
//     redone.
//
// Kind distinguishes "still trying" (transport/429/5xx/interrupted) from the two
// terminal kinds (recovered/giveup), which is all a banner needs: put it up, keep
// it current, take it down.
type RetryNote struct {
	Scope   string        `json:"scope"` // "request" | "turn"
	Kind    string        `json:"kind"`  // transport|429|5xx|interrupted|recovered|giveup
	Attempt int           `json:"attempt"`
	Status  int           `json:"status,omitempty"`
	Delay   time.Duration `json:"delay"`
	Elapsed time.Duration `json:"elapsed"`
	Budget  time.Duration `json:"budget"`
	Reason  string        `json:"reason"`
	Detail  string        `json:"detail,omitempty"` // the server's own message, when it sent one
	// Queue detail from a fair-share proxy's 429 (corrallm sends all of it): slots
	// busy of total, requests ahead, and the proxy's reason. Zero = not reported.
	Capacity int    `json:"capacity,omitempty"`
	InFlight int    `json:"in_flight,omitempty"`
	Waiting  int    `json:"waiting,omitempty"`
	Queue    string `json:"queue,omitempty"` // "rejected"|"queue-timeout"|"spill"|"exhausted"
	// ServerAsked: Delay is the server's own Retry-After, not our backoff guess.
	ServerAsked bool `json:"server_asked,omitempty"`
}

// Queued reports whether the provider described a queue worth showing.
func (n RetryNote) Queued() bool { return n.Capacity > 0 || n.Waiting > 0 }

// Retrying reports whether the note describes a wait in progress (as opposed to
// the recovery or the give-up that ends one).
func (n RetryNote) Retrying() bool { return n.Kind != "recovered" && n.Kind != "giveup" }

// String is the one-line form used for stderr and the TUI's scrollback.
func (n RetryNote) String() string {
	s := n.Reason
	if n.Retrying() && n.Delay > 0 {
		if n.ServerAsked {
			s += fmt.Sprintf(" — the provider asked for %s", n.Delay.Round(time.Second))
		} else {
			s += fmt.Sprintf(" — retrying in %s", n.Delay.Round(time.Second))
		}
	}
	if n.Detail != "" {
		s += ": " + n.Detail
	}
	return s
}

// noteFromEvent converts the client's own retry report into dun's shape.
func noteFromEvent(ev llm.RetryEvent) RetryNote {
	n := RetryNote{
		Scope: "request", Kind: string(ev.Kind), Attempt: ev.Attempt, Status: ev.Status,
		Delay: ev.Delay, Elapsed: ev.Elapsed, Budget: ev.Budget, Reason: ev.Reason,
		Detail: ev.Body, ServerAsked: ev.ServerAsked,
	}
	if ev.Unbounded() {
		n.Budget = 0 // "no ceiling" reads better as absent than as a century
	}
	if bp := ev.BP; bp != nil {
		n.Capacity, n.InFlight, n.Waiting, n.Queue = bp.Capacity, bp.InFlight, bp.Waiting, bp.Reason
	}
	// The reason sentence already quotes the server's message on the paths where
	// there is one (5xx carries it, and a backpressure reason is built from the
	// same fields), so Detail would print it a second time in the same line.
	if n.Detail != "" && strings.Contains(n.Reason, n.Detail) {
		n.Detail = ""
	}
	return n
}

// wireRetry routes an llm.Client's retry narration to cfg.OnRetry.
//
// Type-asserted rather than added to agent.LLMRunner: the hook is a property of
// THIS transport, and a caller who supplies some other runner simply gets no
// request-scope narration (the turn-scope retry below still reports).
func wireRetry(client agent.LLMRunner, onRetry func(RetryNote)) {
	if onRetry == nil {
		return
	}
	if c, ok := client.(*llm.Client); ok {
		c.OnRetry = func(ev llm.RetryEvent) { onRetry(noteFromEvent(ev)) }
	}
}

// applyRetryPolicy lets an operator move the provider-retry policy without
// recompiling, since which values are right depends entirely on the endpoint:
//
//   - DUN_RETRY_BUDGET — wall-clock ceiling for retrying ONE request. A negative
//     duration means unbounded, which is the correct setting for a single-slot
//     local endpoint where a wait is not a delay but another attempt at the only
//     slot, and where the 5m default gives up mid-busy-spell.
//   - DUN_RETRY_5XX_ATTEMPTS — how many times a 5xx is retried. The library
//     default suits a chat turn; a long batch wants more, because one blip
//     outlasting the backoff otherwise fails the whole run.
//
// Both bound each other on purpose: the attempt cap stops a genuinely broken
// upstream, the budget stops a slow one.
func applyRetryPolicy(client agent.LLMRunner) {
	c, ok := client.(*llm.Client)
	if !ok {
		return
	}
	if v := os.Getenv("DUN_RETRY_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RetryBudget = d
			if d < 0 {
				log.Print("dun: DUN_RETRY_BUDGET is negative — retrying provider " +
					"backpressure with no wall-clock ceiling; only the session timeout " +
					"and ctrl+c stop it.")
			}
		} else {
			log.Printf("dun: ignoring invalid DUN_RETRY_BUDGET=%q", v)
		}
	}
	if v := os.Getenv("DUN_RETRY_5XX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retry5xxAttempts = n
		} else {
			log.Printf("dun: ignoring invalid DUN_RETRY_5XX_ATTEMPTS=%q", v)
		}
	}
}

// turnRetryPolicy is the backoff schedule + wall-clock budget for the turn-scope
// retry, taken from the LLM client so both levels obey ONE policy — including an
// operator's Client.RetryBudget override. A runner that is not an *llm.Client
// (a fake, a different transport) gets the same defaults a zero Client would.
//
// DUN_TURN_RETRY_BUDGET overrides the budget alone; "0" disables the turn-scope
// retry entirely (a mid-stream death then fails the turn immediately, as before).
func turnRetryPolicy(client agent.LLMRunner) (initial, max, budget time.Duration) {
	c, ok := client.(*llm.Client)
	if !ok {
		c = &llm.Client{}
	}
	initial, max, budget = c.RetryPolicy()
	if v := os.Getenv("DUN_TURN_RETRY_BUDGET"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return initial, max, budget
		}
		budget = d
	}
	return initial, max, budget
}

// runTurn runs one turn, retrying the whole turn while the provider is the thing
// that failed.
//
// Why this layer exists at all: llm.Client retries everything up to the response
// headers, but once tokens are streaming a death is NOT resumable — readStream
// reports it as a stream error, agent.Session returns it, and before this dun
// printed one line and exited. That is the failure the user sees as "a generic
// connection error and dun dies".
//
// Retrying a turn is cheap and safe because the conversation is PERSISTED: the
// next attempt rebuilds context from the store, so tool calls that already ran
// are not run again — only the interrupted generation is redone. prepareTurn runs
// before every attempt, which is what makes a message typed during the wait ride
// along with the retry instead of waiting for it.
func (h *Harness) runTurn(ctx context.Context, turn func(context.Context) (agent.TurnResult, error)) (agent.TurnResult, error) {
	initial, maxBackoff, budget := turnRetryPolicy(h.client)
	backoff := initial
	start := time.Now()
	deadline := start.Add(budget)
	for attempt := 1; ; attempt++ {
		h.prepareTurn(ctx)
		res, err := turn(ctx)
		if err == nil {
			if attempt > 1 {
				h.retry(RetryNote{Scope: "turn", Kind: "recovered", Attempt: attempt,
					Elapsed: time.Since(start), Budget: budget,
					Reason: fmt.Sprintf("recovered on attempt %d", attempt)})
			}
			return res, nil
		}
		// A cancelled context is the user (or --timeout) saying stop; retrying past
		// it is a bug. Checked explicitly because a cancelled request arrives here
		// wrapped in *url.Error, which satisfies net.Error and so LOOKS transient.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return res, err
		}
		if budget <= 0 || !llm.TransientUpstream(err) {
			return res, err
		}
		if time.Now().Add(backoff).After(deadline) {
			h.retry(RetryNote{Scope: "turn", Kind: "giveup", Attempt: attempt,
				Elapsed: time.Since(start), Budget: budget,
				Reason: fmt.Sprintf("gave up after %s and %d attempts: %v",
					time.Since(start).Round(time.Second), attempt, err)})
			return res, err
		}
		h.retry(RetryNote{Scope: "turn", Kind: "interrupted", Attempt: attempt,
			Delay: backoff, Elapsed: time.Since(start), Budget: budget,
			Reason: fmt.Sprintf("the turn was interrupted — %s", llm.TransientUpstreamReason(err)),
			Detail: err.Error()})
		if !turnRetrySleep(ctx, backoff) {
			return res, ctx.Err()
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// retry delivers a note to the UI hook (nil-safe).
func (h *Harness) retry(n RetryNote) {
	if h.onRetry != nil {
		h.onRetry(n)
	}
}

// turnRetrySleep is the wait between turn attempts. A var so a test can exercise
// the loop without sitting out the real schedule; production never replaces it.
var turnRetrySleep = sleepOrCancel

// sleepOrCancel waits d, or returns false as soon as ctx ends.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
