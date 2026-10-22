package dund

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sign-in tokens (AUTH-2): what a person gets by signing in with the
// identity provider. Opaque, high entropy; only their SHA-256 is kept, with
// whom they stand for, who signed in, and when they lapse. Revocable one at
// a time; deleting the file revokes them all.

const signInPrefix = "dun_s_"

type signIn struct {
	Principal string    `json:"principal"`
	Email     string    `json:"email"`
	Expires   time.Time `json:"expires"`
}

type signIns struct {
	path string
	mu   sync.Mutex
}

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
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := signInPrefix + base64.RawURLEncoding.EncodeToString(b)
	t := s.signIns()
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return "", err
	}
	m[hashToken(tok)] = signIn{Principal: principal, Email: email, Expires: time.Now().Add(ttl)}
	return tok, t.save(m)
}

// CheckSignIn is the principal a live sign-in token stands for.
func (s *Service) CheckSignIn(tok string) (string, bool) {
	if len(tok) <= len(signInPrefix) || tok[:len(signInPrefix)] != signInPrefix {
		return "", false
	}
	t := s.signIns()
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return "", false
	}
	v, ok := m[hashToken(tok)]
	if !ok || time.Now().After(v.Expires) {
		return "", false
	}
	return v.Principal, true
}

// RevokeSignIn ends a sign-in token (signing out).
func (s *Service) RevokeSignIn(tok string) error {
	t := s.signIns()
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return err
	}
	delete(m, hashToken(tok))
	return t.save(m)
}
