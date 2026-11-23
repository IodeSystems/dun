package textfmt

import (
	"strings"
	"time"
)

// ShortDur is d as a person writes it, without its zero units: "30m", "1m30s", "2h".
func ShortDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
