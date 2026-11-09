package dundcli

import (
	"testing"

	"github.com/iodesystems/dun/proto"
)

func TestHitWhere(t *testing.T) {
	for _, c := range []struct {
		h    proto.RagHit
		want string
	}{
		{proto.RagHit{Path: "file:///r/demo.go", StartLine: 3, EndLine: 8, Symbol: "Alpha"}, "/r/demo.go:3-7 Alpha"},
		{proto.RagHit{Path: "file:///r/a.md", StartLine: 5, EndLine: 6, Symbol: "Guide > Install"}, "/r/a.md:5 Guide > Install"},
		{proto.RagHit{Path: "file:///r/scan.pdf", Page: 2}, "/r/scan.pdf p2"},
		{proto.RagHit{Path: "https://x/y"}, "https://x/y"},
	} {
		if got := hitWhere(c.h); got != c.want {
			t.Errorf("hitWhere(%+v) = %q, want %q", c.h, got, c.want)
		}
	}
}
