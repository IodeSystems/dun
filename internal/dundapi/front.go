package dundapi

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/webterm"
)

// The browser's way in (USER 2026-11-17): one listener serves
//
//	/            the chooser: Terminal (/tui) or Web app (/web, experimental)
//	/web/...     the web app (mobile/, built with baseUrl /web)
//	/tui/...     the TUI in a pty (internal/webterm), when tui is on
//	/auth/...    sign-in: /auth/signin (a token), /auth/login (the identity provider)
//
// and the API at root as before (/api/*, /healthz, /releases, /dun.apk).
//
// A browser signs in once and holds a session cookie: HttpOnly,
// SameSite=Strict, Secure over TLS. Its value is a sign-in token (AUTH-2:
// opaque, high entropy, only its SHA-256 kept) made from the bearer the
// person signed in with, and it ends when that bearer does
// (dund.MintSignInFrom). The chooser remembers the choice in a second
// cookie whose value is only "tui" or "web", so / redirects to one of two
// fixed paths and never to a URL a request names.

const (
	sessionCookie = "dun_session"
	choiceCookie  = "dun_choice"
	sessionTTL    = 7 * 24 * time.Hour
	signinPath    = "/auth/signin"
	signoutPath   = "/auth/signout"
	chooseAgain   = "/?choose=1"
)

// openPath: what anyone may reach. /auth is the sign-in itself; hello and
// /healthz say nothing about sessions; releases are a public download, opened
// by the app with Linking.openURL, which carries no credentials.
func openPath(p string) bool {
	switch {
	case p == "/api/v1/hello", p == "/healthz", p == apkPath,
		strings.HasPrefix(p, "/auth/"), strings.HasPrefix(p, releasesPath+"/"):
		return true
	}
	return false
}

// secure: the browser reached dund over TLS (directly, or through hz).
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// cookieSession is the sign-in token in the session cookie, "" for none.
func cookieSession(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// startSession mints a session from the bearer `from` names and sets its
// cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, principal, email, from string) error {
	tok, err := s.svc.MintSignInFrom(principal, email, from, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: secure(r), MaxAge: int(sessionTTL / time.Second)})
	return nil
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: secure(r), MaxAge: -1})
}

// pathReturn is safeReturn without the app's scheme: where a browser sign-in
// may land.
func pathReturn(ret string) string {
	if r, ok := safeReturn(ret); ok && strings.HasPrefix(r, "/") {
		return r
	}
	return "/"
}

func signinURL(ret, why string) string {
	q := url.Values{"return": {ret}}
	if why != "" {
		q.Set("error", why)
	}
	return signinPath + "?" + q.Encode()
}

// hop lands the browser on target with a page of its own, not a redirect: a
// redirect that ends a cross-site chain (back from the identity provider)
// is cross-site to the browser, which then withholds a SameSite=Strict
// cookie; a navigation this page starts is same-site.
func hop(w http.ResponseWriter, target string) {
	t := html.EscapeString(target)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=` + t +
		`"><title>dun</title><p><a id="next" href="` + t + `">Continue</a></p>`))
}

const pageHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>dun</title>
<style>
:root{--bg:#f6f7f9;--fg:#15181d;--muted:#5b6370;--card:#fff;--line:#d9dde3;--accent:#1f6feb}
@media (prefers-color-scheme: dark){:root{--bg:#0d0f12;--fg:#d7dae0;--muted:#8a929e;--card:#151920;--line:#262c35;--accent:#4aa8ff}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif}
main{max-width:30rem;margin:12vh auto;padding:0 16px}
h1{font-size:1.3rem;margin:0 0 1rem}
a.choice{display:block;padding:14px 16px;margin:0 0 10px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--fg);text-decoration:none}
a.choice:hover{border-color:var(--accent)}
.muted{color:var(--muted);font-size:.9em}
.err{color:#d1242f}
input[type=password]{width:100%;box-sizing:border-box;padding:10px;border:1px solid var(--line);border-radius:6px;background:var(--card);color:var(--fg)}
button,.btn{margin-top:10px;padding:8px 14px;border:1px solid var(--accent);border-radius:6px;background:var(--accent);color:#fff;font:inherit;cursor:pointer;text-decoration:none;display:inline-block}
.link{background:none;border:none;color:var(--accent);padding:0;margin:0;cursor:pointer;font:inherit}
</style></head><body><main>`

func page(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write([]byte(pageHead + body + "</main></body></html>"))
}

func (s *Server) tuiOn() bool { return s.term != nil && s.tuiEnabled != nil && s.tuiEnabled() }

// chooser is /: the remembered choice, or the two choices.
func (s *Server) chooser(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("choose") == "" {
		if c, err := r.Cookie(choiceCookie); err == nil {
			switch {
			case c.Value == "tui" && s.tuiOn():
				http.Redirect(w, r, "/tui/", http.StatusFound)
				return
			case c.Value == "web":
				http.Redirect(w, r, "/web/", http.StatusFound)
				return
			}
		}
	}
	b := `<h1>dun</h1>`
	if s.tuiOn() {
		b += `<a class="choice" href="/choose/tui"><b>Terminal</b><br><span class="muted">the dun TUI, in this tab</span></a>`
	}
	b += `<a class="choice" href="/choose/web"><b>Web app</b> <span class="muted">(experimental)</span><br><span class="muted">sessions in a browser layout</span></a>` +
		`<p class="muted">The choice is remembered; come back to ` + chooseAgain + ` to change it.</p>` +
		`<form method="post" action="` + signoutPath + `"><button class="link" type="submit">Sign out</button></form>`
	page(w, b)
}

// choose remembers the choice and goes there.
func (s *Server) choose(w http.ResponseWriter, r *http.Request) {
	to := strings.TrimPrefix(r.URL.Path, "/choose/")
	if to != "web" && (to != "tui" || !s.tuiOn()) {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: choiceCookie, Value: to, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure(r), MaxAge: 365 * 24 * 3600})
	http.Redirect(w, r, "/"+to+"/", http.StatusFound)
}

// signinPage asks for a token (dund token, or a named one), or offers the
// identity provider when it is set up.
func (s *Server) signinPage(w http.ResponseWriter, r *http.Request) {
	ret := pathReturn(r.URL.Query().Get("return"))
	b := `<h1>Sign in to dund</h1>`
	if why := r.URL.Query().Get("error"); why != "" {
		b += `<p class="err">` + html.EscapeString(why) + `</p>`
	}
	if s.sso != nil {
		b += `<p><a class="btn" href="` + html.EscapeString(loginPath+"?return="+url.QueryEscape(ret)) + `">Sign in with ` +
			html.EscapeString(s.sso.cfg.Issuer) + `</a></p><p class="muted">or with a token:</p>`
	}
	b += `<form method="post" action="` + signinPath + `">` +
		`<input type="password" name="token" autocomplete="current-password" placeholder="dun_…" autofocus required>` +
		`<input type="hidden" name="return" value="` + html.EscapeString(ret) + `">` +
		`<button type="submit">Sign in</button></form>` +
		`<p class="muted">Make one on the machine dund runs on: <code>dun dund token</code>, or <code>dun dund token create &lt;name&gt;</code>.</p>`
	page(w, b)
}

// signin takes the token from the form and, when it is live, starts a
// session tied to it.
func (s *Server) signin(w http.ResponseWriter, r *http.Request) {
	if !sameSiteForm(r) {
		http.Error(w, "sign-in from another site", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	ret := pathReturn(r.PostForm.Get("return"))
	tok := strings.TrimSpace(r.PostForm.Get("token"))
	who, ok := s.principalFor(tok)
	if !ok {
		http.Redirect(w, r, signinURL(ret, "that token is not a live dund token"), http.StatusSeeOther)
		return
	}
	if err := s.startSession(w, r, who, "", s.svc.TokenSource(tok)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

// signout ends the session and clears its cookie.
func (s *Server) signout(w http.ResponseWriter, r *http.Request) {
	if !sameSiteForm(r) {
		http.Error(w, "sign-out from another site", http.StatusForbidden)
		return
	}
	if tok := cookieSession(r); tok != "" {
		_ = s.svc.RevokeSignIn(tok)
	}
	clearCookie(w, r, sessionCookie)
	http.Redirect(w, r, signinPath, http.StatusSeeOther)
}

// sameSiteForm: a form post from this site, or from no page at all. A
// browser names the page's origin on a POST; another site's is refused.
func sameSiteForm(r *http.Request) bool {
	return r.Header.Get("Origin") == "" || webterm.SameOrigin(r)
}

// tui is /tui/..., the TUI in a pty, when it is on.
func (s *Server) tui(w http.ResponseWriter, r *http.Request) {
	if !s.tuiOn() {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/tui", s.term).ServeHTTP(w, r)
}

func slash(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := to
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
	}
}
