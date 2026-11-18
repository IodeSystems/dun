package dundapi

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Who the browser is looking at (USER 2026-11-17). dund is reached
// directly (LAN, a VPN's LAN), through hz (a reverse proxy on the box) or
// through cloudflared (a tunnel connector on the box).
//
// The scheme: X-Forwarded-Proto (or Cloudflare's CF-Visitor) is believed
// from loopback and from web.json's trusted_proxies. A client cannot make
// dund think a connection is https when it is not: on the box, only a proxy
// sits between it and the browser.
//
// The host: X-Forwarded-Host (or Forwarded host=) is believed ONLY from a
// peer in trusted_proxies, never from loopback alone: cloudflared connects
// from loopback and passes a client's own X-Forwarded-Host through, so an
// internet client could name any host. cloudflared needs none: it passes the
// public hostname as Host (unless originRequest.httpHostHeader is set). A
// local proxy that rewrites Host and sets X-Forwarded-Host is listed
// explicitly (--trust-proxy 127.0.0.1). With several values the last, the
// closest proxy's, is taken.

// TrustFunc is the trusted proxy list as it is now (web.json changes it
// while dund runs).
type TrustFunc func() []netip.Prefix

func peerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func loopbackPeer(r *http.Request) bool {
	ip, ok := peerAddr(r)
	return ok && ip.IsLoopback()
}

// listedPeer: r came from an address in trusted_proxies.
func listedPeer(r *http.Request, trust TrustFunc) bool {
	ip, ok := peerAddr(r)
	if !ok || trust == nil {
		return false
	}
	for _, p := range trust() {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// proxied: a proxy on the box forwarded r for someone else (it names the
// client). Then r's loopback peer says nothing about where the browser is.
func proxied(r *http.Request) bool {
	return loopbackPeer(r) && (r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("CF-Connecting-IP") != "" ||
		r.Header.Get("Forwarded") != "")
}

// lastValue is the last of a header's comma-separated values, over all its
// lines: the one the closest proxy added.
func lastValue(r *http.Request, name string) string {
	vs := r.Header.Values(name)
	if len(vs) == 0 {
		return ""
	}
	parts := strings.Split(vs[len(vs)-1], ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

// forwardedHost is host= of the last element of Forwarded (RFC 7239).
func forwardedHost(r *http.Request) string {
	el := lastValue(r, "Forwarded")
	for _, pair := range strings.Split(el, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(k, "host") {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// normHost is host in the one spelling dund compares and binds: lower case,
// without a trailing dot on the name (localhost. is localhost), port kept.
func normHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, port, err := net.SplitHostPort(host); err == nil {
		h = strings.TrimSuffix(h, ".")
		if strings.Contains(h, ":") {
			return "[" + h + "]:" + port
		}
		return h + ":" + port
	}
	return strings.TrimSuffix(host, ".")
}

// EffectiveHost is the host the browser is looking at, with its port when
// it has one: X-Forwarded-Host (or Forwarded host=) from a listed proxy,
// else Host. Normalised (normHost).
func EffectiveHost(r *http.Request, trust TrustFunc) string {
	if listedPeer(r, trust) {
		if h := lastValue(r, "X-Forwarded-Host"); h != "" {
			return normHost(h)
		}
		if h := forwardedHost(r); h != "" {
			return normHost(h)
		}
	}
	return normHost(r.Host)
}

// overTLS: the browser's connection is https, directly or (from loopback or
// a listed proxy) as the proxy says.
func overTLS(r *http.Request, trust TrustFunc) bool {
	if r.TLS != nil {
		return true
	}
	if !loopbackPeer(r) && !listedPeer(r, trust) {
		return false
	}
	return strings.EqualFold(lastValue(r, "X-Forwarded-Proto"), "https") ||
		strings.Contains(strings.ReplaceAll(r.Header.Get("CF-Visitor"), " ", ""), `"scheme":"https"`)
}

// OriginCheck is a same-origin check, scheme and host, against what the
// browser is looking at: for the /tui WebSocket and for cookie requests that
// change state. A page on http://H is not https://H's: browsers that do not
// split sites by scheme would otherwise send the cookie from it. A request
// with no Origin is refused.
func OriginCheck(trust TrustFunc) func(*http.Request) bool {
	return func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		if o == "" {
			return false
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return false
		}
		want := "http"
		if overTLS(r, trust) {
			want = "https"
		}
		return strings.EqualFold(u.Scheme, want) && normHost(u.Host) == EffectiveHost(r, trust)
	}
}

// hostName is host without its port or brackets, normalised.
func hostName(host string) string {
	host = normHost(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// sharedLoopback: a name every local server shares (localhost, 127.0.0.1,
// ::1, and 0.0.0.0 / ::, which browsers send to loopback). A cookie is
// scoped to the name, not the port, so every other server on it would get
// dund's sign-in; dun.localhost is a name of dund's own that browsers also
// resolve to loopback.
func sharedLoopback(host string) bool {
	h := hostName(host)
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && (ip.Unmap().IsLoopback() || ip.IsUnspecified())
}

// bareIP is the address when host is an IP that is not loopback: on plain
// http every server on that address gets the cookie. "" otherwise.
func bareIP(host string) string {
	h := hostName(host)
	ip, err := netip.ParseAddr(h)
	if err != nil || ip.Unmap().IsLoopback() || ip.IsUnspecified() {
		return ""
	}
	return h
}

// SharedIPWarning is what a page and `enable tui` say about a bare IP on
// plain http.
func SharedIPWarning(ip string) string {
	return "other servers on " + ip + " receive this sign-in; use a host name, or https through a proxy"
}

// ownLoopback is where a browser on a shared loopback name is sent.
func ownLoopback(host string) string {
	if _, port, err := net.SplitHostPort(normHost(host)); err == nil && port != "" {
		return "dun.localhost:" + port
	}
	return "dun.localhost"
}
