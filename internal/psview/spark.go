package psview

import (
	"strings"
	"time"
)

// sparkSpan is how far back a row's sparkline reaches.
const sparkSpan = time.Minute

// ring is a process's last CPU readings, oldest dropped first.
type ring struct {
	buf  []float64
	head int // where the next reading goes
	n    int
}

func newRing(size int) *ring { return &ring{buf: make([]float64, max(1, size))} }

func (r *ring) push(v float64) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	r.n = min(r.n+1, len(r.buf))
}

// values are the readings, oldest first.
func (r *ring) values() []float64 {
	out := make([]float64, 0, r.n)
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(r.head-r.n+i+len(r.buf))%len(r.buf)])
	}
	return out
}

// ringSize is how many readings at one per every cover sparkSpan.
func ringSize(every time.Duration) int {
	if every <= 0 {
		every = 2 * time.Second
	}
	return max(1, int((sparkSpan+every-1)/every))
}

var sparkGlyphs = []rune("▁▂▃▄▅▆▇█")

// sparkFloor is the least top of a sparkline's scale, in CPU percent: a
// process idling at 0.2% draws flat rather than as noise.
const sparkFloor = 10.0

// sparkline draws vals (oldest first) in width cells, newest at the right;
// more readings than cells are bucketed, each cell its bucket's most.
// Cells with no reading yet are blank.
func sparkline(vals []float64, width int) string {
	if width <= 0 {
		return ""
	}
	cells := vals
	if len(vals) > width {
		cells = make([]float64, width)
		for i := range cells {
			lo, hi := i*len(vals)/width, (i+1)*len(vals)/width
			for _, v := range vals[lo:hi] {
				cells[i] = max(cells[i], v)
			}
		}
	}
	top := sparkFloor
	for _, v := range cells {
		top = max(top, v)
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(cells)))
	for _, v := range cells {
		i := int(max(0, v) / top * float64(len(sparkGlyphs)-1))
		b.WriteRune(sparkGlyphs[min(i, len(sparkGlyphs)-1)])
	}
	return b.String()
}
