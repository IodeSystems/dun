package dun

import (
	"fmt"
	"strconv"
	"strings"
)

// Numbers people read carry thousands separators (USER 2026-10-22): 1234567
// reads at a glance as 1,234,567. This is the one implementation; cmd/dun's
// grouped, commaf and humanBytes call it.

// Grouped writes an integer with thousands separators.
func Grouped[T ~int | ~int32 | ~int64 | ~uint | ~uint32 | ~uint64](n T) string {
	s := strconv.FormatInt(int64(n), 10)
	if n > 0 && uint64(n) > 1<<63-1 { // a uint64 past int64
		s = strconv.FormatUint(uint64(n), 10)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// HumanBytes writes a size the way a person reads one: "2.3 GB", "310 MB",
// "12 KB".
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return Grouped(n/(1<<20)) + " MB"
	case n < 1<<10:
		return Grouped(n) + " B"
	}
	return Grouped(n/(1<<10)) + " KB"
}
