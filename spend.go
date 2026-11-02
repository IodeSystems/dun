package dun

import (
	"path/filepath"

	"github.com/iodesystems/dun/internal/spendmeter"
)

// DefaultLedgerDir is $DUN_HOME/spend.
func DefaultLedgerDir() string { return filepath.Join(DunHome(), "spend") }

// Spend is the session's money and limits; zero-valued when no meter is wired.
func (h *Harness) Spend() spendmeter.Snapshot { return h.cfg.Spend.Snapshot() }

// EndTurnSpend closes the turn's spend (see spendmeter.Meter.EndTurn).
func (h *Harness) EndTurnSpend() { h.cfg.Spend.EndTurn() }
