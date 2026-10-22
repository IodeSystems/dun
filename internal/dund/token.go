package dund

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The local token (AUTH-2): an opaque, high-entropy bearer for clients that
// reach the service over TCP rather than its 0600 socket — a browser on
// localhost, a phone on the LAN. Only its SHA-256 is stored; NewToken
// replaces it and is the only time the token itself exists outside the
// client that keeps it.

func tokenPath(dir string) string { return filepath.Join(dir, "token.sha256") }

// NewToken makes a new local token for the service whose state is in dir,
// replacing any earlier one, and returns it. The service need not be running.
func NewToken(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "dun_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(tok))
	if err := writeFileAtomic(tokenPath(dir), []byte(hex.EncodeToString(sum[:])+"\n")); err != nil {
		return "", fmt.Errorf("dund: token: %w", err)
	}
	return tok, nil
}

// HasToken reports whether a local token has been made.
func (s *Service) HasToken() bool {
	_, err := os.Stat(tokenPath(s.dir))
	return err == nil
}

// CheckToken reports whether tok is the local token.
func (s *Service) CheckToken(tok string) bool {
	want, err := os.ReadFile(tokenPath(s.dir))
	if err != nil || tok == "" {
		return false
	}
	wantSum, err := hex.DecodeString(strings.TrimSpace(string(want)))
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], wantSum) == 1
}

// ErrNoToken: no local token has been made, so no TCP client can get in.
var ErrNoToken = errors.New("no local token yet: run `dun dund token`")
