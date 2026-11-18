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
// through cloudflared (a tunnel connector on the box). A proxy says the
// browser's scheme and host in X-Forwarded-Proto / X-Forwarded-Host; those
// headers are believed only from a trusted peer: loopback (hz and
// cloudflared connect from the box) or an address in web.json's
// trusted_proxies. From anyone else they are ignored, so a client cannot
// claim https or another host.
//
// cloudflared passes the public hostname as Host unless its
// originRequest.httpHostHeader is set; it sends no X-Forwarded-Host. With
// httpHostHeader set to something else (localhost:20003), Host is no longer
// the name the browser used, and the Origin checks below refuse the browser's
// WebSockets and form posts, saying both names.

// TrustFunc is the trusted proxy list as it is now (web.json changes it
// while dund runs).
type TrustFunc func() []netip.Prefix

// trustedPeer: r came from loopback or a trusted proxy.
func trustedPeer(r *http.Request, trust TrustFunc) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	if trust != nil {
		for _, p := range trust() {
			if p.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func firstValue(v string) string {
	v, _, _ = strings.Cut(v, ",")
	return strings.TrimSpace(v)
}

// EffectiveHost is the host the browser is looking at, with its port when
// it has one: X-Forwarded-Host from a trusted peer, else Host. Lower case.
func EffectiveHost(r *http.Request, trust TrustFunc) string {
	if trustedPeer(r, trust) {
		if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" {
			return strings.ToLower(h)
		}
	}
	return strings.ToLower(r.Host)
}

// overTLS: the browser's connection is https, directly or (from a trusted
// peer) as the proxy says. Cloudflare also says it in CF-Visitor.
func overTLS(r *http.Request, trust TrustFunc) bool {
	if r.TLS != nil {
		return true
	}
	if !trustedPeer(r, trust) {
		return false
	}
	return strings.EqualFold(firstValue(r.Header.Get("X-Forwarded-Proto")), "https") ||
		strings.Contains(strings.ReplaceAll(r.Header.Get("CF-Visitor"), " ", ""), `"scheme":"https"`)
}

// OriginCheck is a same-origin check against the effective host: for the
// /tui WebSocket and for cookie requests that change state. A request with
// no Origin is refused.
func OriginCheck(trust TrustFunc) func(*http.Request) bool {
	return func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		if o == "" {
			return false
		}
		u, err := url.Parse(o)
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, EffectiveHost(r, trust))
	}
}

// hostName is host without its port, and without brackets for IPv6.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// sharedLoopback: a loopback name every local server shares ("localhost",
// 127.0.0.1, ::1). A cookie is scoped to the name, not the port, so every
// other server on it would get dund's sign-in; dun.localhost is a name of
// dund's own that browsers also resolve to loopback.
func sharedLoopback(host string) bool {
	h := hostName(host)
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

// bareIP is the address when host is an IP that is not loopback: on plain
// http every server on that address gets the cookie. "" otherwise.
func bareIP(host string) string {
	h := hostName(host)
	ip, err := netip.ParseAddr(h)
	if err != nil || ip.Unmap().IsLoopback() {
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
	if _, port, err := net.SplitHostPort(host); err == nil && port != "" {
		return "dun.localhost:" + port
	}
	return "dun.localhost"
}
