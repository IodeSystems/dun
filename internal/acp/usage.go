package acp

import (
	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// Usage reporting (docs/acp.md, "Usage, context and cost"). dund's usage
// event, sent before each turn's done, carries the session's context
// (window size, tokens in it now), its spend, and the turn's own totals
// (turn_stats). The editor gets:
//
//   - usage_update {size, used, cost}: after each model round (its request
//     done: the context grew) and at the turn's end; cost is the session's
//     cumulative spend and is left out when unknown;
//   - the prompt response's usage: the turn's totals, sent with the stop
//     reason of the turn's end (an unstable ACP field).

// usageState is what the session knows of its usage: the last window and
// context, the cost, and the turn's totals so far.
type usageState struct {
	size, used int64
	cost       *sdk.Cost
	turn       *sdk.Usage // the finished turn's totals; nil until its usage event
	sent       sdk.SessionUsageUpdate
}

// usageCost is the session's spend as an ACP cost: nil when dun has none, or
// when it is not in dollars (ACP's currency is an ISO 4217 code: dun's
// "credits" and a mix of currencies have none). The amount is the sum of the
// costs the gateway reported per response, so an estimate where the gateway
// prices by its own rate card; ACP has no exact/estimate flag.
func usageCost(sp *proto.Spend) *sdk.Cost {
	if sp == nil || sp.Session == nil || sp.Mixed || sp.Currency != "USD" {
		return nil
	}
	return &sdk.Cost{Amount: *sp.Session, Currency: sp.Currency}
}

// turnUsage is a turn's totals as ACP's usage. Input counts the cached part,
// as dun's prompt does. dun does not split out reasoning tokens.
func turnUsage(t *proto.TurnStats) *sdk.Usage {
	if t == nil || t.Rounds == 0 && t.Prompt == 0 && t.Completion == 0 {
		return nil
	}
	u := &sdk.Usage{InputTokens: int64(t.Prompt), OutputTokens: int64(t.Completion), TotalTokens: int64(t.Prompt + t.Completion)}
	if t.Cached > 0 {
		v := int64(t.Cached)
		u.CachedReadTokens = &v
	}
	if t.CacheWrite > 0 {
		v := int64(t.CacheWrite)
		u.CachedWriteTokens = &v
	}
	return u
}

// setContext keeps the usage event's window, context and cost. Callers
// hold s.mu.
func (s *session) setContext(u *proto.Usage) {
	if u.Window.Window > 0 {
		s.usage.size = int64(u.Window.Window)
	}
	s.usage.used = int64(u.Active)
	if u.Spend != nil {
		s.usage.cost = usageCost(u.Spend)
	}
}

// onUsage: the turn's accounting. Sends the context and cost, and keeps the
// turn's totals for the prompt response.
func (s *session) onUsage(u *proto.Usage) {
	s.mu.Lock()
	s.usage.turn = turnUsage(u.TurnStats)
	s.setContext(u)
	s.mu.Unlock()
	s.sendUsage()
}

// onRound: a model round finished; the context is its prompt and reply.
func (s *session) onRound(r *proto.Request) {
	if r.Phase != "done" || r.Prompt == 0 {
		return
	}
	s.mu.Lock()
	if r.Window > 0 {
		s.usage.size = int64(r.Window)
	}
	s.usage.used = int64(r.Prompt + r.Gen)
	s.mu.Unlock()
	s.sendUsage()
}

// loadUsage applies the snapshot's last usage; session/load also sends it.
func (s *session) loadUsage(u *proto.Usage, send bool) {
	s.mu.Lock()
	s.setContext(u)
	s.mu.Unlock()
	if send {
		s.sendUsage()
	}
}

// sendUsage sends a usage_update when it differs from the last one sent and
// the window is known.
func (s *session) sendUsage() {
	s.mu.Lock()
	u := sdk.SessionUsageUpdate{Size: s.usage.size, Used: s.usage.used, Cost: s.usage.cost}
	skip := u.Size == 0 || sameUsage(u, s.usage.sent)
	if !skip {
		s.usage.sent = u
	}
	s.mu.Unlock()
	if !skip {
		s.update(sdk.SessionUpdate{UsageUpdate: &u})
	}
}

func sameUsage(a, b sdk.SessionUsageUpdate) bool {
	if a.Size != b.Size || a.Used != b.Used || (a.Cost == nil) != (b.Cost == nil) {
		return false
	}
	return a.Cost == nil || a.Cost.Amount == b.Cost.Amount && a.Cost.Currency == b.Cost.Currency
}

// takeTurnUsage returns the finished turn's totals and clears them.
func (s *session) takeTurnUsage() *sdk.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.usage.turn
	s.usage.turn = nil
	return u
}
