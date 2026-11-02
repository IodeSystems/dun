package numfmt

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Numbers people read carry thousands separators (USER 2026-10-22): 1234567
// reads at a glance as 1,234,567. This is the one implementation.

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

// Numbers people read — token counts, chars, bytes — carry thousands
// separators: 1234567 reads at a glance as 1,234,567 and not at all as
// 1234567 (USER 2026-10-22). Reports format with commaf instead of
// fmt.Sprintf; ids, ports and years are not written with it.

// Commaf is fmt.Sprintf, except an integer given to %d is written with
// thousands separators (width and the - flag are kept; other flags on %d
// fall back to plain %d).
func Commaf(format string, args ...any) string {
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
		return Grouped(v), true
	case int32:
		return Grouped(v), true
	case int64:
		return Grouped(v), true
	case uint:
		return Grouped(v), true
	case uint32:
		return Grouped(v), true
	case uint64:
		return Grouped(v), true
	}
	return "", false
}

func KTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// Money is an amount with thousands separators and two decimals.
func Money(f float64) string {
	cents := int64(math.Round(f * 100))
	return fmt.Sprintf("%s.%02d", Grouped(cents/100), abs64(cents%100))
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// TokN is a token count in one precision: plain below 1000, then thousands
// with one decimal (1.0k, 18.4k, 128.3k), then millions with two.
func TokN(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1e6)
}

// TokK is a size the short way — a window: 940, 18.4k, 128k, 1.05M.
func TokK(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.2f", float64(n)/1e6), ".00") + "M"
}

// MoneyOf is one amount in its currency.
func MoneyOf(v float64, cur string) string {
	switch cur {
	case llm.CurrencyUSD:
		return "$" + Money(v)
	case llm.CurrencyUSDEstimate:
		return "~$" + Money(v) + " est"
	case "credits":
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + " credits"
	case "":
		return Money(v)
	}
	return Money(v) + " " + cur
}

// EstimateTok is the four-characters-per-token estimate the engine uses
// when it cannot count (dun/context_tokens.go).
func EstimateTok(s string) int { return len(s) / 4 }

// Secs is milliseconds as "41.2s", or "850ms" under a second.
func Secs(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", int(ms))
	}
	return fmt.Sprintf("%.1fs", (time.Duration(ms) * time.Millisecond).Seconds())
}

// Rate is a speed in tokens a second, the short way: "6.7k/s".
func Rate(tps float64) string { return TokK(int(tps)) + "/s" }

// TurnMoney is a turn's cost: a turn is often under a cent, which two
// decimals would show as nothing.
func TurnMoney(v float64, cur string) string {
	if v > 0 && v < 0.01 {
		s := fmt.Sprintf("%.4f", v)
		switch cur {
		case llm.CurrencyUSD:
			return "$" + s
		case llm.CurrencyUSDEstimate:
			return "~$" + s + " est"
		case "":
			return s
		}
		return s + " " + cur
	}
	return MoneyOf(v, cur)
}
