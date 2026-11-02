package tui

import (
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/numfmt"
)

// An estimate says so wherever an amount is shown.
func TestEstimatesSaySo(t *testing.T) {
	if got := numfmt.MoneyOf(1.5, llm.CurrencyUSDEstimate); got != "~$1.50 est" {
		t.Errorf("moneyOf %q", got)
	}
	if got := numfmt.TurnMoney(0.0042, llm.CurrencyUSDEstimate); got != "~$0.0042 est" {
		t.Errorf("turnMoney %q", got)
	}
	if got := moneyIn(map[string]float64{llm.CurrencyUSD: 1, llm.CurrencyUSDEstimate: 2}); got != "$1.00 + ~$2.00 est" {
		t.Errorf("moneyIn %q", got)
	}
}
