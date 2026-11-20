//go:build unix

package dundcli

import (
	"bytes"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/iodesystems/dun/proto"
)

func pairPost(t *testing.T, b *http.Client, u, code string, hdr http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u+pairPath, strings.NewReader(url.Values{"code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		if k == "Host" { // Go sends req.Host, not a Host header
			req.Host = v[0]
			continue
		}
		req.Header[k] = v
	}
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// newBrowser is a browser with no cookies that does not follow redirects.
func newBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: pin},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func pairCode(t *testing.T, st proto.WebState) string {
	t.Helper()
	if st.Pair == nil {
		t.Fatal("no pairing code")
	}
	_, code, ok := strings.Cut(st.Pair.URL, pairPath+"#code=")
	if !ok || code == "" {
		t.Fatalf("pair URL %q", st.Pair.URL)
	}
	return code
}

// --url is saved; --token mints a code that signs one browser in, once.
func TestPairCycle(t *testing.T) {
	c, a1, _ := ctlFixture(t)
	if _, err := c.set(proto.WebSet{Pair: true}); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("pair without a public URL: %v", err)
	}
	if _, err := c.set(proto.WebSet{PublicURL: "https://x.example/path"}); err == nil {
		t.Fatal("a public URL with a path was taken")
	}
	st, err := c.set(proto.WebSet{Enable: ptr(true), PublicURL: "https://Tunnel.Example/", Pair: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.PublicURL != "https://tunnel.example" || !strings.HasPrefix(st.Pair.URL, "https://tunnel.example/auth/pair#code=") ||
		time.Until(st.Pair.Expires) < 9*time.Minute {
		t.Fatalf("state %+v %+v", st, st.Pair)
	}
	if saved, _, _ := loadWebState(c.home); saved.PublicURL != "https://tunnel.example" {
		t.Errorf("web.json: %+v", saved)
	}
	// Later calls need no --url.
	st2, err := c.set(proto.WebSet{Enable: ptr(true), Pair: true})
	if err != nil || st2.Pair == nil || st2.Pair.URL == st.Pair.URL {
		t.Fatalf("second code: %v %+v", err, st2)
	}
	code := pairCode(t, st)

	phone := newBrowser()
	if got := status(t, phone, dl(a1)+"/api/v1/sessions"); got != 401 {
		t.Fatalf("before pairing: %d", got)
	}
	if got := status(t, phone, dl(a1)+pairPath); got != 200 {
		t.Fatalf("pair page: %d", got)
	}
	if r := pairPost(t, phone, dl(a1), "wrong", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("wrong code: %d", r.StatusCode)
	}
	r := pairPost(t, phone, dl(a1), code, nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/web/" {
		t.Fatalf("pair: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if got := status(t, phone, dl(a1)+"/api/v1/sessions"); got != 200 {
		t.Fatalf("after pairing: %d", got)
	}
	other := newBrowser()
	if r := pairPost(t, other, dl(a1), code, nil); r.StatusCode != http.StatusGone {
		t.Errorf("used code: %d", r.StatusCode)
	}
	if got := status(t, other, dl(a1)+"/api/v1/sessions"); got != 401 {
		t.Errorf("a used code signed in: %d", got)
	}

	// Through cloudflared on the box: loopback peer, Host the public name,
	// X-Forwarded-Proto https. The cookie is the __Host- one.
	code2 := pairCode(t, st2)
	r = pairPost(t, newBrowser(), "http://"+a1, code2, http.Header{
		"Host": {"tunnel.example"}, "X-Forwarded-For": {"203.0.113.9"}, "X-Forwarded-Proto": {"https"},
		"Origin": {"https://tunnel.example"}})
	if r.StatusCode != http.StatusSeeOther || len(r.Cookies()) != 1 || r.Cookies()[0].Name != "__Host-dun_session" || !r.Cookies()[0].Secure {
		t.Errorf("tunnel pair: %d %v", r.StatusCode, r.Cookies())
	}

	// Turning web off ends the codes.
	st3, err := c.set(proto.WebSet{Pair: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.set(proto.WebSet{Enable: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.svc.PairDevice(pairCode(t, st3)); err == nil {
		t.Error("a code outlived disable web")
	}
	if _, err := c.set(proto.WebSet{Pair: true}); err == nil || !strings.Contains(err.Error(), "listener on") {
		t.Errorf("pair with web off: %v", err)
	}
}

// The QR is the link: its half blocks read back to the encoder's bitmap.
func TestQRText(t *testing.T) {
	u := "https://restored-creating-arabia-sun.trycloudflare.com/auth/pair#code=AAAAAAAAAAAAAAAAAAAAAAAAAAA"
	s, err := qrText(u)
	if err != nil || s == "" {
		t.Fatal(err)
	}
	q, _ := qrcode.New(u, qrcode.Medium)
	want := q.Bitmap()
	var got [][]bool
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		line = strings.TrimSuffix(strings.TrimPrefix(line, "\x1b[30;47m"), "\x1b[0m")
		var top, bot []bool
		for _, r := range line {
			top = append(top, r == '█' || r == '▀')
			bot = append(bot, r == '█' || r == '▄')
		}
		got = append(got, top, bot)
	}
	got = got[:len(want)] // an odd row count leaves a blank half
	if !reflect.DeepEqual(got, want) {
		t.Error("the QR text is not the code's bitmap")
	}
	var out bytes.Buffer
	if err := printPair(&out, proto.WebPair{URL: u, Expires: time.Now().Add(10 * time.Minute)}); err != nil ||
		!strings.Contains(out.String(), u) || !strings.Contains(out.String(), "█") {
		t.Errorf("printPair: %v\n%s", err, out.String())
	}
}
