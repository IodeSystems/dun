package tui

import (
	"testing"
	"unsafe"
)

// TestBaseline_TuiModelSize records the by-value copy cost of tuiModel for
// the pointer-ization plan's before/after measurement.
func TestBaseline_TuiModelSize(t *testing.T) {
	t.Logf("unsafe.Sizeof(tuiModel{}) = %d bytes", unsafe.Sizeof(tuiModel{}))
}
