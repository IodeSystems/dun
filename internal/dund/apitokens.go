package dund

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Named API tokens (AUTH-2): bearer tokens a person makes one per client
// (a script, a phone) and revokes by name. Each stands for the owner, like the
// local token; there are no scopes. Only the SHA-256 is kept, in api-tokens.json
// (0600, written atomically). The file is read on every check, so a revocation
// by the CLI takes effect without restarting dund.

const apiTokenPrefix = "dun_n_"

// ErrTokenExists: a named token of that name is already made.
var ErrTokenExists = errors.New("a token with that name exists")

// ErrNoSuchToken: no named token of that name.
var ErrNoSuchToken = errors.New("no token with that name")

var tokenName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// APIToken is what is kept about a named token: never the token itself.
type APIToken struct {
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

var apiTokensMu sync.Mutex

func apiTokensPath(dir string) string { return filepath.Join(dir, "api-tokens.json") }

func loadAPITokens(dir string) (map[string]APIToken, error) {
	b, err := os.ReadFile(apiTokensPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]APIToken{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]APIToken{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("dund: api tokens: %w", err)
	}
	return m, nil
}

func saveAPITokens(dir string, m map[string]APIToken) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(apiTokensPath(dir), b)
}

// CreateAPIToken makes the token called name for the service whose state is in
// dir and returns it: the only time it exists outside the client. A name in
// use is refused. The service need not be running.
func CreateAPIToken(dir, name string) (string, error) {
	if !tokenName.MatchString(name) {
		return "", fmt.Errorf("dund: token name %q: use letters, digits, . _ - (up to 64)", name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	apiTokensMu.Lock()
	defer apiTokensMu.Unlock()
	m, err := loadAPITokens(dir)
	if err != nil {
		return "", err
	}
	if _, ok := m[name]; ok {
		return "", fmt.Errorf("dund: token %q: %w", name, ErrTokenExists)
	}
	m[name] = APIToken{Name: name, Hash: hashToken(tok), Created: time.Now().UTC().Truncate(time.Second)}
	if err := saveAPITokens(dir, m); err != nil {
		return "", fmt.Errorf("dund: token: %w", err)
	}
	return tok, nil
}

// ListAPITokens is the named tokens, by name.
func ListAPITokens(dir string) ([]APIToken, error) {
	apiTokensMu.Lock()
	defer apiTokensMu.Unlock()
	m, err := loadAPITokens(dir)
	if err != nil {
		return nil, err
	}
	out := make([]APIToken, 0, len(m))
	for _, t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RevokeAPIToken ends the token called name.
func RevokeAPIToken(dir, name string) error {
	apiTokensMu.Lock()
	defer apiTokensMu.Unlock()
	m, err := loadAPITokens(dir)
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return fmt.Errorf("dund: token %q: %w", name, ErrNoSuchToken)
	}
	delete(m, name)
	return saveAPITokens(dir, m)
}

// CheckAPIToken reports whether tok is a live named token.
func (s *Service) CheckAPIToken(tok string) bool {
	if len(tok) <= len(apiTokenPrefix) || tok[:len(apiTokenPrefix)] != apiTokenPrefix {
		return false
	}
	apiTokensMu.Lock()
	defer apiTokensMu.Unlock()
	m, err := loadAPITokens(s.dir)
	if err != nil {
		return false
	}
	got := []byte(hashToken(tok))
	ok := false
	for _, t := range m {
		if subtle.ConstantTimeCompare(got, []byte(t.Hash)) == 1 {
			ok = true
		}
	}
	return ok
}
