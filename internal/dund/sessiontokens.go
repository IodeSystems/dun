package dund

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sign-in tokens (AUTH-2): what a person gets by signing in with the
// identity provider. Opaque, high entropy; only their SHA-256 is kept, with
// whom they stand for, who signed in, and when they lapse. Revocable one at
// a time; deleting the file revokes them all.
//
// A browser session (dundapi's session cookie) is a sign-in token too, minted
// from the bearer the person signed in with. From names that bearer by its
// hash ("local:<sha256>", "api:<sha256>", "signin:<sha256>"; "" for a sign-in
// with the identity provider), and the session is live only while that bearer
// is: `dund token` (a new local token), `dund token revoke` and a sign-out
// end the sessions made from them, at their next check.

const signInPrefix = "dun_s_"

type signIn struct {
	Principal string    `json:"principal"`
	Email     string    `json:"email"`
	Expires   time.Time `json:"expires"`
	From      string    `json:"from,omitempty"`
	// Host, for a browser session, is the host it was made on (the name
	// the browser was looking at, with its port): it is good there only
	// (CheckSessionAt), never as a bearer.
	Host string `json:"host,omitempty"`
}

type signIns struct{ path string }

// signInsMu guards sign-ins.json for every Service in the process: a
// lock per signIns value (one is made per call) guarded nothing.
var signInsMu sync.Mutex

func (s *Service) signIns() *signIns { return &signIns{path: filepath.Join(s.dir, "sign-ins.json")} }

func (t *signIns) load() (map[string]signIn, error) {
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]signIn{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]signIn{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("dund: sign-ins: %w", err)
	}
	return m, nil
}

func (t *signIns) save(m map[string]signIn) error {
	now := time.Now()
	for k, v := range m {
		if now.After(v.Expires) {
			delete(m, k) // lapsed ones go on the next write
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(t.path, b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// MintSignIn makes a sign-in token for principal (who signed in as email),
// good for ttl.
func (s *Service) MintSignIn(principal, email string, ttl time.Duration) (string, error) {
	return s.MintSignInFrom(principal, email, "", ttl)
}

// MintSignInFrom is MintSignIn for a token that lives only while from does
// (TokenSource's name for the bearer it was made from; "" for none).
func (s *Service) MintSignInFrom(principal, email, from string, ttl time.Duration) (string, error) {
	return s.mint(signIn{Principal: principal, Email: email, From: from}, ttl)
}

// MintSessionAt is MintSignInFrom for a browser session on host: it is
// accepted only by CheckSessionAt with the same host, so a cookie made for
// one name is refused when it arrives under another (another port on
// 127.0.0.1, another name for the machine).
func (s *Service) MintSessionAt(principal, email, from, host string, ttl time.Duration) (string, error) {
	if host == "" {
		return "", errors.New("dund: a browser session needs its host")
	}
	return s.mint(signIn{Principal: principal, Email: email, From: from, Host: strings.ToLower(host)}, ttl)
}

func (s *Service) mint(v signIn, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := signInPrefix + base64.RawURLEncoding.EncodeToString(b)
	t := s.signIns()
	signInsMu.Lock()
	defer signInsMu.Unlock()
	m, err := t.load()
	if err != nil {
		return "", err
	}
	v.Expires = time.Now().Add(ttl)
	m[hashToken(tok)] = v
	return tok, t.save(m)
}

// CheckSignIn is the principal a live sign-in token stands for. A browser
// session (MintSessionAt) is not a bearer: it is refused here.
func (s *Service) CheckSignIn(tok string) (string, bool) { return s.checkSignIn(tok, "", false) }

// CheckSessionAt is the principal a live browser session made on host
// stands for; a session made on another host, or a sign-in token with no
// host, is refused.
func (s *Service) CheckSessionAt(tok, host string) (string, bool) {
	if host == "" {
		return "", false
	}
	return s.checkSignIn(tok, strings.ToLower(host), true)
}

func (s *Service) checkSignIn(tok, host string, session bool) (string, bool) {
	if len(tok) <= len(signInPrefix) || tok[:len(signInPrefix)] != signInPrefix {
		return "", false
	}
	t := s.signIns()
	signInsMu.Lock()
	defer signInsMu.Unlock()
	m, err := t.load()
	if err != nil {
		return "", false
	}
	v, ok := m[hashToken(tok)]
	if !ok || time.Now().After(v.Expires) || !s.fromLive(m, v.From, 0) ||
		subtle.ConstantTimeCompare([]byte(v.Host), []byte(host)) != 1 || (v.Host != "") != session {
		return "", false
	}
	return v.Principal, true
}

// TokenSource names a live bearer for MintSignInFrom: the local token, a
// named API token or a sign-in token, by its hash. "" when tok is none of
// them.
func (s *Service) TokenSource(tok string) string {
	switch {
	case s.CheckToken(tok):
		return "local:" + hashToken(tok)
	case s.CheckAPIToken(tok):
		return "api:" + hashToken(tok)
	}
	if _, ok := s.CheckSignIn(tok); ok {
		return "signin:" + hashToken(tok)
	}
	return ""
}

// fromLive reports whether the bearer a sign-in was made from is still live
// (m is the sign-ins, already loaded under their lock). A chain of sign-ins
// is followed a few steps, no further.
func (s *Service) fromLive(m map[string]signIn, from string, depth int) bool {
	kind, hash, _ := strings.Cut(from, ":")
	switch kind {
	case "":
		return true
	case "local":
		want, err := os.ReadFile(tokenPath(s.dir))
		return err == nil && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(want))), []byte(hash)) == 1
	case "api":
		apiTokensMu.Lock()
		defer apiTokensMu.Unlock()
		toks, err := loadAPITokens(s.dir)
		if err != nil {
			return false
		}
		for _, t := range toks {
			if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash)) == 1 {
				return true
			}
		}
		return false
	case "signin":
		v, ok := m[hash]
		return ok && depth < 3 && !time.Now().After(v.Expires) && s.fromLive(m, v.From, depth+1)
	}
	return false
}

// RevokeSignIn ends a sign-in token (signing out).
func (s *Service) RevokeSignIn(tok string) error {
	t := s.signIns()
	signInsMu.Lock()
	defer signInsMu.Unlock()
	m, err := t.load()
	if err != nil {
		return err
	}
	delete(m, hashToken(tok))
	return t.save(m)
}
