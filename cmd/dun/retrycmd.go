package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// /retry: how hard dun keeps at a flaky provider (USER 2026-10-26: "my corp
// llm is incredibly flaky"). Three knobs, all agentkit's, applied to the
// running client at once and saved to config.json for every session:
//
//	attempts  how many times a 5xx is retried (default 5)
//	budget    the wall clock one request may spend retrying 429s and 5xxs
//	          (default 5m; "unbounded": until the turn's own timeout)
//	turn      the wall clock for re-running a turn whose stream died midway
//	          (default: the request's budget; "off": no re-run)
//
// The backoff between tries is agentkit's own: 1s, doubling, to 30s, with
// jitter, and a provider's Retry-After wins over it.

type retryPreset struct {
	name, about  string
	attempts     int
	budget, turn time.Duration
}

var retryPresets = []retryPreset{
	{"default", "5 tries · a request up to 5m · a dead stream re-run for 5m", 0, 0, 0},
	{"patient", "10 tries · a request up to 15m · a dead stream re-run for 30m", 10, 15 * time.Minute, 30 * time.Minute},
	{"stubborn", "20 tries · a request up to 1h · a dead stream re-run for 2h", 20, time.Hour, 2 * time.Hour},
}

// retryLine is the client's retry settings in words.
func retryLine(c *llm.Client) string {
	attempts := c.Retry5xxAttempts
	if attempts <= 0 {
		attempts = 5
	}
	budget := "5m"
	switch {
	case c.RetryBudget < 0:
		budget = "unbounded"
	case c.RetryBudget > 0:
		budget = c.RetryBudget.String()
	}
	turn := "as long as a request"
	switch {
	case c.DisableTurnRetry:
		turn = "off"
	case c.TurnRetryBudget > 0:
		turn = c.TurnRetryBudget.String()
	}
	return fmt.Sprintf("retry: a 5xx up to %d tries · a request retries for up to %s · a turn whose stream died re-runs for %s · backoff 1s doubling to 30s (or the provider's Retry-After)",
		attempts, budget, turn)
}

// retryCmd runs /retry's action on the client and returns what to say and
// the config to save (nil when nothing changed).
func retryCmd(c *llm.Client, action string) (string, *dunConfig, error) {
	f := strings.Fields(strings.ToLower(action))
	if len(f) == 0 || f[0] == "status" {
		return retryLine(c), nil, nil
	}
	for _, p := range retryPresets {
		if f[0] == p.name && len(f) == 1 {
			c.Retry5xxAttempts, c.RetryBudget, c.TurnRetryBudget, c.DisableTurnRetry = p.attempts, p.budget, p.turn, false
			return retryLine(c) + " (" + p.name + ", saved)", saveRetry(c), nil
		}
	}
	if len(f) != 2 {
		return "", nil, fmt.Errorf("usage: /retry [default|patient|stubborn | attempts N | budget <duration>|unbounded|default | turn <duration>|off|default]")
	}
	switch f[0] {
	case "attempts":
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > 1000 {
			return "", nil, fmt.Errorf("attempts: a number from 1 to 1000")
		}
		c.Retry5xxAttempts = n
	case "budget":
		switch f[1] {
		case "unbounded":
			c.RetryBudget = -1
		case "default":
			c.RetryBudget = 0
		default:
			d, err := time.ParseDuration(f[1])
			if err != nil || d <= 0 {
				return "", nil, fmt.Errorf("budget: a duration (90s, 15m, 1h), unbounded or default")
			}
			c.RetryBudget = d
		}
	case "turn":
		switch f[1] {
		case "off":
			c.TurnRetryBudget, c.DisableTurnRetry = 0, true
		case "default":
			c.TurnRetryBudget, c.DisableTurnRetry = 0, false
		default:
			d, err := time.ParseDuration(f[1])
			if err != nil || d <= 0 {
				return "", nil, fmt.Errorf("turn: a duration (5m, 1h), off or default")
			}
			c.TurnRetryBudget, c.DisableTurnRetry = d, false
		}
	default:
		return "", nil, fmt.Errorf("unknown /retry %q", f[0])
	}
	return retryLine(c) + " (saved)", saveRetry(c), nil
}

// saveRetry is the config with the client's retry settings in it, in the
// fields parseRetryBudgets reads.
func saveRetry(c *llm.Client) *dunConfig {
	fc := loadConfig()
	fc.Retry5xxAttempts = c.Retry5xxAttempts
	fc.RetryBudget = ""
	switch {
	case c.RetryBudget < 0:
		fc.RetryBudget = "-1s"
	case c.RetryBudget > 0:
		fc.RetryBudget = c.RetryBudget.String()
	}
	fc.TurnRetryBudget = ""
	switch {
	case c.DisableTurnRetry:
		fc.TurnRetryBudget = "0"
	case c.TurnRetryBudget > 0:
		fc.TurnRetryBudget = c.TurnRetryBudget.String()
	}
	return &fc
}
