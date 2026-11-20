package dund

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Pairing codes (USER 2026-11-20, docs/decisions.md "Pair a phone by QR"):
// `dun dund enable web --token` asks the running dund for a one-time code,
// shown as a QR of <public_url>/auth/pair#code=<code>. The phone's browser
// redeems it once (dundapi pair.go) for a browser session, as a sign-in
// with a token would make. Codes live in dund's memory only, as their
// SHA-256: a restart ends them all, which a 10-minute code can afford.

// PairTTL is how long a pairing code is good for.
const PairTTL = 10 * time.Minute

// pairMaxFails is how many wrong codes end every live code: 160 random bits
// cannot be guessed, so a stream of wrong ones is a client to stop.
const pairMaxFails = 20

var (
	ErrPairWrong   = errors.New("that pairing code is not one dund made")
	ErrPairUsed    = errors.New("that pairing code was used already; each works once")
	ErrPairExpired = errors.New("that pairing code expired")
)

type pairCode struct {
	hash    string
	expires time.Time
	used    bool
}

type pairing struct {
	mu    sync.Mutex
	codes []pairCode // used and expired ones stay an hour, to say why they fail
	fails int
	now   func() time.Time // nil: time.Now; tests set it
}

func (s *Service) pairing() *pairing { return &s.pair }

func (p *pairing) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// MintPairCode makes a one-time pairing code, good for PairTTL.
func (s *Service) MintPairCode() (code string, expires time.Time, err error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code = base64.RawURLEncoding.EncodeToString(b)
	p := s.pairing()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	kept := p.codes[:0]
	for _, c := range p.codes {
		if now.Before(c.expires.Add(time.Hour)) {
			kept = append(kept, c)
		}
	}
	expires = now.Add(PairTTL)
	p.codes = append(kept, pairCode{hash: hashToken(code), expires: expires})
	p.fails = 0
	return code, expires, nil
}

// DropPairCodes ends every pairing code (the web listener was turned off).
func (s *Service) DropPairCodes() {
	p := s.pairing()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes, p.fails = nil, 0
}

// redeemPairCode marks code used. It is refused once used, once expired,
// and when wrong; pairMaxFails wrong ones end every code.
func (s *Service) redeemPairCode(code string) error {
	got := []byte(hashToken(code))
	p := s.pairing()
	p.mu.Lock()
	defer p.mu.Unlock()
	at := -1
	for i, c := range p.codes {
		if subtle.ConstantTimeCompare(got, []byte(c.hash)) == 1 {
			at = i
		}
	}
	switch {
	case at < 0 || code == "":
		if p.fails++; p.fails >= pairMaxFails {
			for i := range p.codes {
				p.codes[i].used = true
			}
			p.fails = 0
		}
		return ErrPairWrong
	case p.codes[at].used:
		return ErrPairUsed
	case !p.clock().Before(p.codes[at].expires):
		return ErrPairExpired
	}
	p.codes[at].used = true
	return nil
}

// PairDevice redeems code and makes the device's credential: a named API
// token, "paired-<date>-via-qr-<4 hex>", whose secret is not kept by anyone.
// It is the session's anchor: from (TokenSource's form) is what the browser
// session is minted from, so `dun dund token revoke <name>` ends it, and
// `token list` shows it.
func (s *Service) PairDevice(code string) (from, name string, err error) {
	if err := s.redeemPairCode(code); err != nil {
		return "", "", err
	}
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	name = fmt.Sprintf("paired-%s-via-qr-%s", time.Now().Format("2006-01-02"), hex.EncodeToString(b))
	tok, err := CreateAPIToken(s.dir, name)
	if err != nil {
		return "", "", err
	}
	return "api:" + hashToken(tok), name, nil
}
