package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iodesystems/dun/internal/update"
)

// Store is the registry on disk ($DUN_HOME/registry: index.json and its
// signature, as fetched) and where to fetch it from. What is on disk is
// verified on every read, as it was when it was fetched: a file changed
// there is refused, not trusted.
type Store struct {
	Dir    string
	URL    string // the index; its signature at URL + ".minisig"
	Keys   []update.PublicKey
	Fetch  *update.Fetcher
	MaxAge time.Duration    // a cached index younger than this is not fetched again
	Now    func() time.Time // for tests
}

const (
	indexFile = "index.json"
	sigFile   = "index.json.minisig"
)

// DefaultMaxAge is how often dun asks for the registry: daily.
const DefaultMaxAge = 24 * time.Hour

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Load is the cached index, verified; os.ErrNotExist when there is none.
func (s *Store) Load() (*Index, error) {
	body, err := os.ReadFile(filepath.Join(s.Dir, indexFile))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(s.Dir, sigFile))
	if err != nil {
		return nil, err
	}
	return Verify(body, string(sig), s.Keys)
}

// Fresh reports whether the cached index was fetched within MaxAge.
func (s *Store) Fresh() bool {
	st, err := os.Stat(filepath.Join(s.Dir, sigFile))
	return err == nil && s.now().Sub(st.ModTime()) < s.MaxAge
}

// Refresh fetches the index unless the cached one is fresh, verifies it and
// keeps it, and returns what is in force. An index published before the
// cached one is not kept (a replayed old file cannot roll dun back).
// changed is true when a new index was kept.
func (s *Store) Refresh(ctx context.Context) (ix *Index, changed bool, err error) {
	cached, cerr := s.Load()
	if cerr == nil && s.Fresh() {
		return cached, false, nil
	}
	if s.URL == "" {
		return cached, false, errors.New("registry: no address")
	}
	body, err := s.Fetch.Small(ctx, s.URL, MaxIndex)
	if err != nil {
		return cached, false, fmt.Errorf("registry: %w", err)
	}
	sig, err := s.Fetch.Small(ctx, s.URL+".minisig", 4<<10)
	if err != nil {
		return cached, false, fmt.Errorf("registry: %w", err)
	}
	got, err := Verify(body, string(sig), s.Keys)
	if err != nil {
		return cached, false, err
	}
	if cerr == nil && got.PublishedAt().Before(cached.PublishedAt()) {
		s.touch()
		return cached, false, fmt.Errorf("registry: %s is older than the one kept (%s); kept that one", got.Published, cached.Published)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return got, false, err
	}
	if err := writeAtomic(filepath.Join(s.Dir, indexFile), body); err != nil {
		return got, false, err
	}
	// The signature last: Fresh reads its time, and a crash between the
	// two leaves a pair that does not verify, which is fetched again.
	if err := writeAtomic(filepath.Join(s.Dir, sigFile), sig); err != nil {
		return got, false, err
	}
	return got, cerr != nil || got.Published != cached.Published, nil
}

// touch marks the cache as just checked.
func (s *Store) touch() {
	now := s.now()
	_ = os.Chtimes(filepath.Join(s.Dir, sigFile), now, now)
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
