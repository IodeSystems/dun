package dundapi_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// Pages on a shared loopback name are sent to dun.localhost (proxy.go), so
// the tests browse there: dun.localhost:<port> is dialled as 127.0.0.1:<port>
// whatever the resolver says.
func TestMain(m *testing.M) {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if rest, ok := strings.CutPrefix(addr, "dun.localhost:"); ok {
			addr = "127.0.0.1:" + rest
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	http.DefaultTransport.(*http.Transport).DialContext = dial
	websocket.DefaultDialer.NetDialContext = dial
	os.Exit(m.Run())
}

// dl is srv's URL under dund's own loopback name.
func dl(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "127.0.0.1", "dun.localhost", 1)
}
