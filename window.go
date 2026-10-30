package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// The context window, when the server will not state it (USER 2026-10-26,
// critical: "dun doesn't learn CTX length, no way to set it"). llama.cpp
// states it at /props; an OpenAI-compatible gateway (OpenRouter, LiteLLM,
// vLLM) does not have /props, so dun ran those sessions with no shaping and
// no generation cap. In order:
//
//	DUN_CONTEXT_TOKENS                     this run
//	the window set for this model          /context config --max N, saved in $DUN_HOME/context.json
//	the server's /props                    llama.cpp, corrallm
//	the model list                         /models (OpenRouter context_length, vLLM max_model_len),
//	                                       /model/info (LiteLLM max_input_tokens)

// Endpoint is the model in use and where it is: the key the saved window
// is filed under, and what the model list is asked with.
type Endpoint struct {
	URL, Key, Model string
}

func (e Endpoint) windowKey() string {
	return strings.TrimRight(e.URL, "/") + " " + e.Model
}

// ContextConfig is /context config's settings for one model (USER
// 2026-10-26): 0 is "not set", which leaves dun's default or what the server
// states. The environment still wins (DUN_CONTEXT_TOKENS,
// DUN_MAX_OUTPUT_TOKENS).
type ContextConfig struct {
	// Max is the context window, in tokens.
	Max int `json:"max,omitempty"`
	// CompactBuffer is the room kept for the reply, in tokens: the prompt is
	// compacted when it would leave less (default 32,768).
	CompactBuffer int `json:"compact_buffer,omitempty"`
	// ToolLOD is the length, in characters, past which an older tool result
	// is sent as a stub (default 4,000).
	ToolLOD int `json:"tool_lod,omitempty"`
	// RecapNudge is the conversation size, in tokens, at which dun reminds
	// the model it can recap (default 20,000).
	RecapNudge int `json:"recap_nudge,omitempty"`
}

func contextFile() string { return filepath.Join(DunHome(), "context.json") }

func loadContexts() map[string]ContextConfig {
	m := map[string]ContextConfig{}
	if b, err := os.ReadFile(contextFile()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// savedContext is e's model's settings.
func savedContext(e Endpoint) ContextConfig {
	if e.Model == "" {
		return ContextConfig{}
	}
	return loadContexts()[e.windowKey()]
}

// saveContext files c for e's model; an all-zero c removes it.
func saveContext(e Endpoint, c ContextConfig) error {
	m := loadContexts()
	if c == (ContextConfig{}) {
		delete(m, e.windowKey())
	} else {
		m[e.windowKey()] = c
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(DunHome(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(contextFile(), append(b, '\n'), 0o600)
}

// savedWindow is the window set for e, 0 when none is.
func savedWindow(e Endpoint) int { return savedContext(e).Max }

// The settings in force in this process (one session's harness per engine;
// its children share them), read where each one decides.
var ctxOverride struct {
	compactBuffer, toolLOD, recapNudge atomic.Int64
}

// applyContextConfig puts c's buffer, stub length and nudge in force.
func applyContextConfig(c ContextConfig) {
	ctxOverride.compactBuffer.Store(int64(c.CompactBuffer))
	ctxOverride.toolLOD.Store(int64(c.ToolLOD))
	ctxOverride.recapNudge.Store(int64(c.RecapNudge))
}

// defaultToolLOD is the stub length when none is set.
const defaultToolLOD = 4000

// lodChars is the tool-result stub length in force.
func lodChars() int {
	if n := ctxOverride.toolLOD.Load(); n > 0 {
		return int(n)
	}
	return defaultToolLOD
}

// recapNudgeAt is the recap reminder's size in force.
func recapNudgeAt() int {
	if n := ctxOverride.recapNudge.Load(); n > 0 {
		return int(n)
	}
	return recapNudgeTokens
}

var windowHTTP = &http.Client{Timeout: contextWindowTimeout}

// getListed reads one of e's model-list endpoints into v.
func getListed(ctx context.Context, e Endpoint, u string, v any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	if e.Key != "" {
		req.Header.Set("Authorization", "Bearer "+e.Key)
	}
	resp, err := windowHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return err == nil && json.Unmarshal(b, v) == nil
}

// listedWindow is the window e's model list states for its model: the
// OpenAI-style /models (OpenRouter, vLLM), else LiteLLM's /model/info.
func listedWindow(ctx context.Context, e Endpoint) int {
	if e.URL == "" || e.Model == "" {
		return 0
	}
	base := strings.TrimRight(e.URL, "/")
	get := func(u string, v any) bool { return getListed(ctx, e, u, v) }
	var models struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			MaxModelLen   int    `json:"max_model_len"`
			ContextWindow int    `json:"context_window"`
			TopProvider   struct {
				ContextLength int `json:"context_length"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if get(base+"/models", &models) {
		for _, m := range models.Data {
			if m.ID != e.Model {
				continue
			}
			for _, n := range []int{m.ContextLength, m.TopProvider.ContextLength, m.MaxModelLen, m.ContextWindow} {
				if n > 0 {
					return n
				}
			}
		}
	}
	var info struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				MaxInputTokens int `json:"max_input_tokens"`
				MaxTokens      int `json:"max_tokens"`
			} `json:"model_info"`
		} `json:"data"`
	}
	for _, u := range []string{base + "/model/info", strings.TrimSuffix(base, "/v1") + "/model/info"} {
		if !get(u, &info) {
			continue
		}
		for _, m := range info.Data {
			if m.ModelName != e.Model {
				continue
			}
			if m.ModelInfo.MaxInputTokens > 0 {
				return m.ModelInfo.MaxInputTokens
			}
			if m.ModelInfo.MaxTokens > 0 {
				return m.ModelInfo.MaxTokens
			}
		}
	}
	return 0
}

// SetWindow sets this session's context window and files it for the model
// (/context config --max N); 0 forgets the saved one and asks again. It
// arms shaping and the generation cap from the next build.
func (h *Harness) SetWindow(ctx context.Context, n int) (string, error) {
	e := h.cfg.Endpoint
	if n < 0 {
		return "", fmt.Errorf("a window is a number of tokens (131072, 128k) or auto")
	}
	if e.Model != "" {
		c := savedContext(e)
		c.Max = n
		if err := saveContext(e, c); err != nil {
			return "", err
		}
	}
	if n > 0 {
		h.window.Store(int64(n))
		h.windowRunner, h.windowSrc = nil, "the window set for "+e.Model+" (/context config --max)"
		return fmt.Sprintf("context window: %s tokens for %s (saved; compaction and the response cap use it from the next request)", Grouped(n), e.Model), nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*contextWindowTimeout)
	defer cancel()
	got, src := h.askWindow(cctx, h.cfg.Client)
	h.window.Store(int64(got))
	h.windowSrc = src
	if got == 0 {
		h.windowRunner, h.windowAsks, h.windowSkip = h.cfg.Client, 0, 0
		return "context window: auto — the server states none; /context config --max <tokens> sets it", nil
	}
	h.windowRunner = nil
	return fmt.Sprintf("context window: auto — %s tokens, from %s", Grouped(got), src), nil
}

// WindowStatus is the window in force and where it came from.
func (h *Harness) WindowStatus() (int, string) { return h.windowTokens(), h.windowSrc }

// ParseTokens reads "131072", "128k" or "1m". k and m are thousands and
// millions: a window read a little small compacts a little early, one read
// large (128k as 131,072 for a 128,000 model) overflows.
func ParseTokens(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, ",", "")))
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1000*1000, strings.TrimSuffix(s, "m")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a number of tokens", s)
	}
	return n * mult, nil
}

// ContextKeys are /context config's settings, as its flags name them.
var ContextKeys = []string{"max", "compact-buffer", "tool-lod", "recap-nudge"}

// ConfigureContext sets some of /context config's settings for this model
// and saves them: changes maps a ContextKeys name to its value, 0 for the
// default (max: ask the server again). They are in force from the next
// request.
func (h *Harness) ConfigureContext(ctx context.Context, changes map[string]int) (string, error) {
	e := h.cfg.Endpoint
	c := savedContext(e)
	for k, v := range changes {
		if v < 0 {
			return "", fmt.Errorf("--%s: a number, or default", k)
		}
		switch k {
		case "max":
			// SetWindow saves it and resolves the window now.
		case "compact-buffer":
			if v > 0 && v < minOutputTokens {
				return "", fmt.Errorf("--compact-buffer: at least %s tokens (a reply needs room)", Grouped(minOutputTokens))
			}
			c.CompactBuffer = v
		case "tool-lod":
			if v > 0 && v < 200 {
				return "", fmt.Errorf("--tool-lod: at least 200 characters")
			}
			c.ToolLOD = v
		case "recap-nudge":
			c.RecapNudge = v
		default:
			return "", fmt.Errorf("unknown setting --%s (%s)", k, strings.Join(ContextKeys, ", "))
		}
	}
	if e.Model != "" {
		c.Max = savedWindow(e)
		if err := saveContext(e, c); err != nil {
			return "", err
		}
	}
	applyContextConfig(c)
	if v, ok := changes["max"]; ok {
		if _, err := h.SetWindow(ctx, v); err != nil {
			return "", err
		}
	}
	h.writeHarnessInfo()
	return h.ContextStatus() + " (saved for " + e.Model + "; from the next request)", nil
}

// ContextStatus is the settings in force, in words.
func (h *Harness) ContextStatus() string {
	n, src := h.WindowStatus()
	max := "unknown (set it: --max)"
	if n > 0 {
		max = Grouped(n) + " tokens, from " + src
	}
	return fmt.Sprintf("context: max %s · compact-buffer %s · tool-lod %s chars · recap-nudge %s",
		max, Grouped(maxOutputTokens()), Grouped(lodChars()), Grouped(recapNudgeAt()))
}

// ContextSettings is the settings in force, for a client.
func (h *Harness) ContextSettings() (max int, maxFrom string, compactBuffer, toolLOD, recapNudge int) {
	n, src := h.WindowStatus()
	return n, src, maxOutputTokens(), lodChars(), recapNudgeAt()
}

// NewTestHarness is a harness with only an endpoint, for a client's tests of
// /context config.
func NewTestHarness(e Endpoint) *Harness { return &Harness{cfg: Config{Endpoint: e}} }
