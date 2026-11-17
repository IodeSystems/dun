package replay

import (
	"os"
	"strconv"

	"github.com/iodesystems/dun/internal/dunconfig"
	shim "github.com/iodesystems/openai-claude-api-shim"
)

// defaultPrices is a small fallback table for the providers dun's recorded
// sessions used. The user's $DUN_HOME/prices.json override (via
// dunconfig.LoadPrices) always wins; this table is for when no override file
// exists, so the replay still reports a cost rather than zero.
//
// Rates are USD per million tokens. Anthropic rates come from the shim's
// built-in table; Fireworks is a flat-rate provider (no cache read/write
// distinction) so Read = Input and Write5m = Write1h = Input.
var defaultPrices = shim.Prices{
	// Fireworks: flat rate, no cache tiers. The rate used by the harbor runner
	// (DUN_PRICE_IN/DUN_PRICE_CACHED/DUN_PRICE_OUT) is 3.0/0.3/15 per 1M.
	"accounts/fireworks/models/": {Input: 3, Write5m: 3, Write1h: 3, Read: 0.3, Output: 15},
	// Anthropic models are in the shim's built-in table; nothing to add here.
}

// LoadPrices returns the price table for the replay: the user's override (from
// $DUN_HOME/prices.json) merged over the default table. User entries win for
// any model they name; the default table fills the rest.
func LoadPrices() shim.Prices {
	user := dunconfig.LoadPrices()
	if len(user) == 0 {
		return defaultPrices
	}
	merged := make(shim.Prices, len(defaultPrices)+len(user))
	for k, v := range defaultPrices {
		merged[k] = v
	}
	for k, v := range user {
		merged[k] = v
	}
	return merged
}

// CostFor returns the USD cost of one round's usage at the given prices, or 0
// when the model has no entry in the table.
//
// The usage is the provider's own token counts for the round: prompt tokens
// (minus cached), cached read, and generated. Cache write is 0 for a replay
// (the replay does not write to the provider's cache; the prefix-kept figure
// is what the provider would have read).
func CostFor(prices shim.Prices, model string, prompt, cached, gen int) float64 {
	usage := shim.Usage{
		Input:     prompt - cached,
		CacheRead: cached,
		Output:    gen,
	}
	cost, ok := shim.Estimate(prices, model, usage)
	if !ok {
		return 0
	}
	return cost
}

// CostEnv reads the harbor runner's flat-rate env vars and returns a Prices
// table that applies them to any model. DUN_PRICE_IN, DUN_PRICE_CACHED, and
// DUN_PRICE_OUT are USD per million tokens. When none are set, returns nil.
func CostEnv() shim.Prices {
	in := envFloat("DUN_PRICE_IN")
	cached := envFloat("DUN_PRICE_CACHED")
	out := envFloat("DUN_PRICE_OUT")
	if in == 0 && cached == 0 && out == 0 {
		return nil
	}
	if in == 0 {
		in = 3
	}
	if cached == 0 {
		cached = 0.3
	}
	if out == 0 {
		out = 15
	}
	return shim.Prices{
		"": {Input: in, Write5m: in, Write1h: in, Read: cached, Output: out},
	}
}

func envFloat(key string) float64 {
	s := os.Getenv(key)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
