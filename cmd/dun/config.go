package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
)

// Config — persisted LLM settings so you don't re-pass --url/--model/--key every
// run. `dun -config` runs the wizard (re-run it any time to reconfigure);
// settings save to $DUN_HOME/config.json (default ~/.dun/config.json, 0600).
//
// Precedence when dun starts: CLI flag > env (DUN_URL/DUN_MODEL/DUN_LLM_KEY) >
// config file > built-in default. So a one-off `--model X` still wins for a
// single run without touching the saved config.

const (
	defaultURL   = "https://llm.iodesystems.com"
	defaultModel = "ternary-bonsai-27b"
)

type dunConfig struct {
	URL   string `json:"url,omitempty"`
	Model string `json:"model,omitempty"`
	Key   string `json:"key,omitempty"`
}

func dunHome() string {
	if h := os.Getenv("DUN_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dun"
	}
	return filepath.Join(home, ".dun")
}

func configPath() string { return filepath.Join(dunHome(), "config.json") }

// loadConfig reads the saved config; a missing/corrupt file yields a zero config.
func loadConfig() dunConfig {
	var c dunConfig
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c dunConfig) error {
	if err := os.MkdirAll(dunHome(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), append(b, '\n'), 0o600) // key is secret
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// The wizard itself is a Bubble Tea program — see setup.go (runSetupTUI).

func maskKey(k string) string {
	if k == "" {
		return "(none)"
	}
	if len(k) <= 4 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}

// fetchModels queries an OpenAI-compatible /models endpoint (best-effort).
func fetchModels(base, key string) []string {
	base = strings.TrimRight(base, "/")
	client := &http.Client{Timeout: 6 * time.Second}
	for _, u := range []string{base + "/v1/models", base + "/models"} {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		ids := decodeModels(resp)
		if len(ids) > 0 {
			return ids
		}
	}
	return nil
}

func decodeModels(resp *http.Response) []string {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// shipConfig resolves the ship policy for this run: the workspace's `ship`
// section, with --pr overriding the default mode.
//
// --pr predates ship modes and used to be its own tool. It survives as
// shorthand because a flag people have in their muscle memory should not stop
// working — but it now means "ship, in pr mode", which is the same pipeline
// with a different terminal state rather than a second way to push.
// clientCache builds LLM runners by model name, one per model, for the lifetime
// of the process.
//
// Cached rather than constructed per call because a client owns a retry policy
// and its backpressure state: two clients on the same model would each think
// they had the endpoint to themselves and would back off independently against
// one shared rate limit. Children on the same model must contend through ONE
// client, which is also why there is no concurrency cap on sub-agents — the
// client is where the queueing actually happens.
func clientCache(url, key string) func(string) (agent.LLMRunner, error) {
	var mu sync.Mutex
	cache := map[string]agent.LLMRunner{}
	return func(model string) (agent.LLMRunner, error) {
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("no model named")
		}
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[model]; ok {
			return c, nil
		}
		c := llm.NewClient(url, key, model)
		cache[model] = c
		return c, nil
	}
}

func shipConfig(workspace string, pr bool) *dun.ShipConfig {
	cfg := dun.LoadShip(workspace)
	if !pr {
		return cfg
	}
	if cfg == nil {
		cfg = &dun.ShipConfig{}
	}
	cfg.Default = dun.ShipPR
	// A flag that asks for a mode the config forbids is a contradiction the
	// user has to see: honour the flag, since it is the more specific
	// statement, rather than silently shipping in some other mode.
	if !shipModeListed(cfg.Allow, dun.ShipPR) && len(cfg.Allow) > 0 {
		cfg.Allow = append(cfg.Allow, dun.ShipPR)
	}
	return cfg
}

func shipModeListed(modes []dun.ShipMode, want dun.ShipMode) bool {
	for _, m := range modes {
		if m == want {
			return true
		}
	}
	return false
}
