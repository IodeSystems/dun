package dundapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/iodesystems/dun/internal/dund"
)

// Sign-in with an OpenID Connect provider (Zitadel), the way corrallm does
// it (USER 2026-10-21): dund runs the login itself.
//
//	GET /auth/login?return=/path | dun://auth   → the provider, with PKCE
//	GET /auth/callback?code&state               → back from it
//
// The callback exchanges the code (client id in the body — a client with no
// secret is refused Basic auth), verifies the ID token (audience = client
// id, nonce, a verified email on the allow-list), and mints a dund sign-in
// token for the owner: every allowed person sees the owner's sessions, so a
// phone sees what the TUI sees. The token goes back in the URL fragment —
// never a query, which servers and proxies log — to /signin on the web, or
// to the dun:// link the app opened the login with.

const (
	signInTTL  = 30 * 24 * time.Hour
	stateTTL   = 10 * time.Minute
	appScheme  = "dun://"
	loginPath  = "/auth/login"
	callbackRP = "/auth/callback"
)

// OIDC is the provider config. Empty Issuer: sign-in is off.
type OIDC struct {
	Issuer   string
	ClientID string
	// Allow is the emails that may sign in.
	Allow []string
	// Redirect overrides the callback URL; default: /auth/callback on the
	// address the browser used (X-Forwarded-Proto/-Host behind hz).
	Redirect string
}

func (o OIDC) on() bool { return o.Issuer != "" && o.ClientID != "" }

type pending struct {
	verifier, nonce, ret, redirect string
	expires                        time.Time
}

type sso struct {
	cfg   OIDC
	svc   *dund.Service
	owner string

	mu       sync.Mutex
	provider *oidc.Provider
	states   map[string]pending
}

func newSSO(cfg OIDC, svc *dund.Service, owner string) *sso {
	return &sso{cfg: cfg, svc: svc, owner: owner, states: map[string]pending{}}
}

// discover finds the provider, tolerating a trailing slash on the issuer
// (go-oidc matches it byte for byte; corrallm lost an afternoon to it).
func (s *sso) discover(ctx context.Context) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		return s.provider, nil
	}
	p, err := oidc.NewProvider(ctx, s.cfg.Issuer)
	if err != nil {
		alt := strings.TrimSuffix(s.cfg.Issuer, "/")
		if alt == s.cfg.Issuer {
			alt += "/"
		}
		if p2, err2 := oidc.NewProvider(ctx, alt); err2 == nil {
			p, err = p2, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("identity provider %s: %w", s.cfg.Issuer, err)
	}
	s.provider = p
	return p, nil
}

func (s *sso) oauth(p *oidc.Provider, redirect string) *oauth2.Config {
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInParams // no secret: client_id in the body
	return &oauth2.Config{ClientID: s.cfg.ClientID, Endpoint: ep, RedirectURL: redirect,
		Scopes: []string{oidc.ScopeOpenID, "email", "profile"}}
}

// redirectFor is the callback on the address the browser used.
func (s *sso) redirectFor(r *http.Request) string {
	if s.cfg.Redirect != "" {
		return s.cfg.Redirect
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host + callbackRP
}

// safeReturn accepts where a sign-in may land: a path on this site or the
// app's own scheme. Anything else would make dund an open redirect that hands
// out tokens.
func safeReturn(ret string) (string, bool) {
	switch {
	case ret == "":
		return "/", true
	case strings.HasPrefix(ret, appScheme):
		return ret, true
	case strings.HasPrefix(ret, "/") && !strings.HasPrefix(ret, "//") && !strings.Contains(ret, "\\"):
		return ret, true
	}
	return "", false
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *sso) login(w http.ResponseWriter, r *http.Request) {
	ret, ok := safeReturn(r.URL.Query().Get("return"))
	if !ok {
		http.Error(w, "return must be a path on this site or dun://", http.StatusBadRequest)
		return
	}
	p, err := s.discover(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	st := pending{verifier: oauth2.GenerateVerifier(), nonce: randomString(), ret: ret,
		redirect: s.redirectFor(r), expires: time.Now().Add(stateTTL)}
	state := randomString()
	s.mu.Lock()
	for k, v := range s.states {
		if time.Now().After(v.expires) {
			delete(s.states, k)
		}
	}
	s.states[state] = st
	s.mu.Unlock()
	http.Redirect(w, r, s.oauth(p, st.redirect).AuthCodeURL(state, oidc.Nonce(st.nonce),
		oauth2.S256ChallengeOption(st.verifier)), http.StatusFound)
}

func (s *sso) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	st, ok := s.states[q.Get("state")]
	delete(s.states, q.Get("state")) // one use
	s.mu.Unlock()
	if !ok || time.Now().After(st.expires) {
		s.fail(w, r, st.ret, "the sign-in expired or was already used — start again")
		return
	}
	if e := q.Get("error"); e != "" {
		s.fail(w, r, st.ret, "the identity provider said: "+e+" "+q.Get("error_description"))
		return
	}
	tok, err := s.finish(r.Context(), st, q.Get("code"))
	if err != nil {
		s.fail(w, r, st.ret, err.Error())
		return
	}
	http.Redirect(w, r, landing(st.ret, url.Values{"token": {tok}}), http.StatusFound)
}

// finish turns the code into a dund sign-in token, or says why not.
func (s *sso) finish(ctx context.Context, st pending, code string) (string, error) {
	p, err := s.discover(ctx)
	if err != nil {
		return "", err
	}
	tok, err := s.oauth(p, st.redirect).Exchange(ctx, code, oauth2.VerifierOption(st.verifier))
	if err != nil {
		return "", fmt.Errorf("could not exchange the sign-in code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", errors.New("the identity provider sent no ID token")
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: s.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("the ID token did not verify: %w", err)
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil {
		return "", err
	}
	if idt.Nonce != st.nonce {
		return "", errors.New("the ID token is not for this sign-in (nonce)")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !claims.Verified {
		return "", errors.New("the account has no verified email")
	}
	if !slices.ContainsFunc(s.cfg.Allow, func(a string) bool { return strings.EqualFold(strings.TrimSpace(a), email) }) {
		return "", fmt.Errorf("%s may not sign in to this dund", email)
	}
	return s.svc.MintSignIn(s.owner, email, signInTTL)
}

// fail lands back where the sign-in started, with why in the fragment.
func (s *sso) fail(w http.ResponseWriter, r *http.Request, ret, why string) {
	if ret == "" {
		ret = "/"
	}
	http.Redirect(w, r, landing(ret, url.Values{"error": {why}}), http.StatusFound)
}

// landing is where a sign-in returns: the app's dun:// link, or the web's
// sign-in page (which keeps the token and goes on to ret), with the result
// in the fragment.
func landing(ret string, frag url.Values) string {
	if strings.HasPrefix(ret, appScheme) {
		return ret + "#" + frag.Encode()
	}
	frag.Set("return", ret)
	return "/signin#" + frag.Encode()
}
