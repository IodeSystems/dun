package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
