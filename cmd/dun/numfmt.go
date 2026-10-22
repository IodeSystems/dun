package main

import (
	"fmt"
	"strings"

	dun "github.com/iodesystems/dun"
)

// Numbers people read — token counts, chars, bytes — carry thousands
// separators: 1234567 reads at a glance as 1,234,567 and not at all as
// 1234567 (USER 2026-10-22). Reports format with commaf instead of
// fmt.Sprintf; ids, ports and years are not written with it.

// grouped writes an integer with thousands separators (dun.Grouped, the one
// implementation).
func grouped[T ~int | ~int32 | ~int64 | ~uint | ~uint32 | ~uint64](n T) string {
	return dun.Grouped(n)
}

// commaf is fmt.Sprintf, except an integer given to %d is written with
// thousands separators (width and the - flag are kept; other flags on %d
// fall back to plain %d).
func commaf(format string, args ...any) string {
	var out strings.Builder
	ai := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			out.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("+-# 0", format[j]) >= 0 {
			j++
		}
		for j < len(format) && (format[j] >= '0' && format[j] <= '9' || format[j] == '.') {
			j++
		}
		if j >= len(format) {
			out.WriteString(format[i:])
			break
		}
		spec, verb := format[i:j+1], format[j]
		i = j
		if verb == '%' {
			out.WriteByte('%')
			continue
		}
		if ai >= len(args) {
			out.WriteString(spec)
			continue
		}
		a := args[ai]
		ai++
		if verb == 'd' && !strings.ContainsAny(spec[1:len(spec)-1], "+# 0.") {
			if s, ok := commaInt(a); ok {
				out.WriteString(fmt.Sprintf(spec[:len(spec)-1]+"s", s))
				continue
			}
		}
		out.WriteString(fmt.Sprintf(spec, a))
	}
	return out.String()
}

// commaInt is grouped for any integer value, and false for anything else.
func commaInt(a any) (string, bool) {
	switch v := a.(type) {
	case int:
		return grouped(v), true
	case int32:
		return grouped(v), true
	case int64:
		return grouped(v), true
	case uint:
		return grouped(v), true
	case uint32:
		return grouped(v), true
	case uint64:
		return grouped(v), true
	}
	return "", false
}
