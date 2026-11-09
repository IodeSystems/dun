package dun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// capstore.go — dun's copy of what agentkit learns about each (endpoint,
// model): the capability record (agentkit llm/capabilities.go). agentkit keeps
// only the Client's in-memory record and reports each change through OnLearn;
// dun keeps the record in $DUN_HOME/capabilities, one file per (endpoint,
// model), and seeds every Client it makes from it, so a refusal is paid for
// once, not once per session (USER 2026-11-07: agentkit learns, the host
// stores).
//
// The provider's effort_off (dunconfig.ProviderConfig.EffortOff) is pinned as
// a host fact: learning never replaces it.

// CapsDir is where the records are kept.
func CapsDir() string { return filepath.Join(DunHome(), "capabilities") }

// capsFile is the record's file for (endpoint, model). Hashed: an endpoint is
// a URL and a model id may hold slashes; the file itself names both.
func capsFile(endpoint, model string) string { return capsFileIn(CapsDir(), endpoint, model) }

func capsFileIn(dir, endpoint, model string) string {
	sum := sha256.Sum256([]byte(endpoint + "\n" + model))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

// capsMu serializes writes: OnLearn runs on request goroutines, and a parent
// and its children may share an (endpoint, model).
var capsMu sync.Mutex

// capsStores counts the background stores OnLearn starts.
var capsStores sync.WaitGroup

// WaitCapsStores returns when every background store TrackCaps started has
// finished. A test calls it before its store directory is removed.
func WaitCapsStores() { capsStores.Wait() }

// LoadCaps is the stored record for (endpoint, model); empty when none.
func LoadCaps(endpoint, model string) llm.Capabilities {
	return LoadCapsIn(CapsDir(), endpoint, model)
}

// LoadCapsIn is LoadCaps from the store in dir: for dund, whose home is its
// --home, not $DUN_HOME.
func LoadCapsIn(dir, endpoint, model string) llm.Capabilities {
	capsMu.Lock()
	defer capsMu.Unlock()
	return loadCapsLocked(dir, endpoint, model)
}

func loadCapsLocked(dir, endpoint, model string) llm.Capabilities {
	c := llm.Capabilities{Endpoint: endpoint, Model: model, Facts: map[string]llm.Fact{}}
	b, err := os.ReadFile(capsFileIn(dir, endpoint, model))
	if err != nil {
		return c
	}
	var stored llm.Capabilities
	if json.Unmarshal(b, &stored) != nil || stored.Endpoint != endpoint || stored.Model != model {
		return c // a hash collision or a damaged file: start over
	}
	if stored.Facts != nil {
		c.Facts = stored.Facts
	}
	return c
}

// storeSnapshot writes c's whole record, as it is now, to the store. Taken
// under capsMu, so whichever write runs last writes the latest state: two
// changes to one key in quick succession cannot leave the older value on disk.
// Host facts are not stored: they come from the config on every start, and a
// stored one would outlive its config line.
func storeSnapshot(c *llm.Client) error {
	capsMu.Lock()
	defer capsMu.Unlock()
	return storeLocked(CapsDir(), c.Capabilities())
}

// StoreCapsIn writes the record snap to the store in dir, replacing the one
// kept for its (endpoint, model). Host facts are not stored (storeSnapshot).
// For a record a probe built outside a Client's traffic: dund's model roles
// (internal/ragd/probe.go).
func StoreCapsIn(dir string, snap llm.Capabilities) error {
	capsMu.Lock()
	defer capsMu.Unlock()
	return storeLocked(dir, snap)
}

func storeLocked(dir string, snap llm.Capabilities) error {
	snap.Facts = maps.Clone(snap.Facts)
	for k, f := range snap.Facts {
		if f.Source == llm.SourceHost {
			delete(snap.Facts, k)
		}
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := capsFileIn(dir, snap.Endpoint, snap.Model)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// TrackCaps seeds c from the store, pins off as the provider's effort_off
// (when set), and stores what c learns from here on. Call it once per Client,
// before its first request: the main client and every child client.
func TrackCaps(c *llm.Client, effortOff string) {
	snap := c.Capabilities()
	stored := LoadCaps(snap.Endpoint, snap.Model)
	if effortOff != "" {
		stored.Facts[llm.CapReasoningOff] = llm.Fact{Value: effortOff, Source: llm.SourceHost,
			Evidence: "provider config effort_off (or DUN_EFFORT_OFF)", At: time.Now()}
	}
	c.SetCapabilities(stored)
	c.OnLearn = func(l llm.Learned) {
		// OnLearn must not block the request, nor re-enter the Client:
		// the snapshot is taken in the background, under the store's lock.
		capsStores.Add(1)
		go func() {
			defer capsStores.Done()
			if err := storeSnapshot(c); err != nil {
				log.Printf("dun: capabilities: not stored: %v", err)
			}
		}()
		log.Printf("dun: capabilities: %s %s: %s = %q (%s: %s)", l.Endpoint, l.Model, l.Key, l.Fact.Value, l.Fact.Source, l.Fact.Evidence)
	}
	// A translation changes what goes on the wire; it is never silent.
	c.OnTranslate = func(t llm.Translation) {
		log.Printf("dun: capabilities: sent %s=%q (asked %q)", t.Key, t.Sent, t.Wanted)
	}
}

// CapsReport is the record as /model shows it: one line per fact, with where
// it came from and when.
func CapsReport(c llm.Capabilities) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s", c.Model, c.Endpoint)
	if len(c.Facts) == 0 {
		b.WriteString("\nnothing known yet: dun learns from traffic and refusals; /model probe checks now (a few short requests)")
		return b.String()
	}
	keys := make([]string, 0, len(c.Facts))
	for k := range c.Facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := c.Facts[k]
		fmt.Fprintf(&b, "\n%-28s %-14s %s, %s", k, f.Value, f.Source, f.At.Local().Format("2006-01-02 15:04"))
		if f.Evidence != "" {
			fmt.Fprintf(&b, " · %s", f.Evidence)
		}
	}
	return b.String()
}
