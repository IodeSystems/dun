package dundapi

import (
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/iodesystems/dun/internal/dund"
)

// Pairing a phone (USER 2026-11-20, docs/decisions.md "Pair a phone by QR"):
// `dun dund enable web --token` shows a QR of <public_url>/auth/pair#code=….
// The code is in the fragment, which the browser never sends: no proxy or
// tunnel logs it. GET /auth/pair is a page whose script moves the code into
// a form; the person taps Sign in, and POST /auth/pair redeems it once
// (dund pair.go) for a browser session, the same cookie /auth/signin sets.
// The tap is there for link previewers that run scripts: a preview must
// not spend the code.

const pairPath = "/auth/pair"

func (s *Server) pairPage(w http.ResponseWriter, r *http.Request) {
	page(w, `<h1>Sign in this device</h1>`+s.ipWarning(r)+
		`<form id="pair" method="post" action="`+pairPath+`" hidden>`+
		`<input type="hidden" name="code" id="code">`+
		`<p>This signs this browser in to dund, as its owner.</p>`+
		`<button type="submit">Sign in</button></form>`+
		`<p id="none" class="err" hidden>This link has no pairing code. Make one with <code>dun dund enable web --token</code>.</p>`+
		`<noscript><p class="err">Pairing needs JavaScript.</p></noscript>`+
		`<script>(function(){var m=/(?:^|&)code=([^&]+)/.exec(location.hash.slice(1));`+
		`history.replaceState(null,"",location.pathname);`+
		`if(m){document.getElementById("code").value=decodeURIComponent(m[1]);document.getElementById("pair").hidden=false}`+
		`else{document.getElementById("none").hidden=false}})()</script>`)
}

// pair redeems the code and starts a session from the device's anchor token.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	if !s.sameSiteForm(r) {
		http.Error(w, "pairing from another site", http.StatusForbidden)
		return
	}
	if h := s.host(r); !s.secure(r) && !proxied(r) && sharedLoopback(h) {
		http.Error(w, "pair at http://"+ownLoopback(h)+"/ — every local server shares "+hostName(h), http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	from, _, err := s.svc.PairDevice(strings.TrimSpace(r.PostForm.Get("code")))
	if err != nil {
		code := http.StatusForbidden
		if errors.Is(err, dund.ErrPairUsed) || errors.Is(err, dund.ErrPairExpired) {
			code = http.StatusGone
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.WriteHeader(code)
		page(w, `<h1>Not signed in</h1><p class="err">`+html.EscapeString(err.Error())+`.</p>`+
			`<p class="muted">Make a new code on the machine dund runs on: <code>dun dund enable web --token</code>.</p>`)
		return
	}
	if err := s.startSession(w, r, s.owner, "", from); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/web/", http.StatusSeeOther)
}
