package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/iodesystems/dun"
	dunreg "github.com/iodesystems/dun/internal/registry"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// The registry (docs/registry.md) for this process: the cached index is
// installed once at start, before anything reads a manifest; an engine
// fetches a fresher one in the background, for the next start.
//
//	DUN_REGISTRY=off     neither (the tests set it)
//	DUN_REGISTRY_URL=…   another index (staging, a test server)

// registryStore is $DUN_HOME/registry and where the index is, or nil when
// the registry is off.
func registryStore() *dunreg.Store {
	if os.Getenv("DUN_REGISTRY") == "off" {
		return nil
	}
	keys, err := update.ParseKeys(release.RegistryKey)
	if err != nil {
		return nil
	}
	// A next key the update check took on (key rotation) signs registries too.
	for _, l := range update.LoadState(update.StateFile(dunHome())).Keys {
		if k, err := update.ParsePublicKey(l); err == nil {
			keys = append(keys, k)
		}
	}
	u := release.RegistryURL
	if v := os.Getenv("DUN_REGISTRY_URL"); v != "" {
		u = v
	}
	return &dunreg.Store{Dir: filepath.Join(dunHome(), "registry"), URL: u, Keys: keys,
		Fetch: &update.Fetcher{}, MaxAge: dunreg.DefaultMaxAge}
}

// useRegistry installs the cached index; a missing or bad one is none.
func useRegistry() {
	s := registryStore()
	if s == nil {
		return
	}
	if ix, err := s.Load(); err == nil {
		dun.UseRegistry(ix, version)
	}
}

// refreshRegistry fetches the index when the cached one is a day old, for
// the next start. Quiet: dun works on what it was built with.
func refreshRegistry(ctx context.Context) {
	s := registryStore()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, _, _ = s.Refresh(ctx)
}
