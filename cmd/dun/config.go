package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
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
// config file. So a one-off `--model X` still wins for a single run without
// touching the saved config.
//
// There is no built-in provider (USER 2026-10-22): dun talks to nothing until
// a provider is logged in (/login, `dun --setup`) or named by flag or env.

// providerOpenAI is the one kind of provider today: any OpenAI-compatible
// endpoint (/v1/models, /v1/chat/completions), with an optional bearer key.
// Specific providers (OpenRouter, Anthropic, Azure…) come later as kinds.
const providerOpenAI = "openai"

type dunConfig struct {
	URL   string `json:"url,omitempty"`
	Model string `json:"model,omitempty"`
	Key   string `json:"key,omitempty"`
	// Providers are the endpoints /model lists models from, each with its own
	// key. Provider names the one in use. URL and Key above stay the active
	// provider's, written on every switch, so a config with no providers (or
	// a binary that predates them) still means what it says.
	Provider  string           `json:"provider,omitempty"`
	Providers []providerConfig `json:"providers,omitempty"`
	// Provider-retry budgets, persisted so an endpoint that needs them does not
	// need the env vars on every launch. Precedence: CLI flag > env
	// (DUN_RETRY_BUDGET / DUN_TURN_RETRY_BUDGET) > these fields > the built-in
	// defaults. Empty = unset; a negative value means unbounded (request scope).
	RetryBudget     string `json:"retry_budget,omitempty"`
	TurnRetryBudget string `json:"turn_retry_budget,omitempty"` // "0" disables turn-scope retry
	// VisionModel describes images for a model that cannot read them (a model
	// id, on the active provider unless VisionProvider names another
	// logged-in one). Empty: a message with images to such a model is
	// refused. See images.go.
	VisionModel    string `json:"vision_model,omitempty"`
	VisionProvider string `json:"vision_provider,omitempty"`
}

// providerConfig is one OpenAI-compatible endpoint: /v1/models and
// /v1/chat/completions under URL, authorized by Key.
type providerConfig struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // "" is providerOpenAI
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
}

// providerList returns the logged-in providers. A config from before
// providers that names a URL has one, the top-level URL and Key, named after
// its host. A config with neither has none: logged out.
func (c dunConfig) providerList() []providerConfig {
	if len(c.Providers) > 0 {
		return c.Providers
	}
	if c.URL == "" {
		return nil
	}
	return []providerConfig{{Name: hostName(c.URL), Kind: providerOpenAI, URL: c.URL, Key: c.Key}}
}

// active returns the provider in use: the one Provider names, else the one
// at the top-level URL, else the first; false when logged out.
func (c dunConfig) active() (providerConfig, bool) {
	ps := c.providerList()
	if len(ps) == 0 {
		return providerConfig{}, false
	}
	for _, p := range ps {
		if p.Name == c.Provider {
			return p, true
		}
	}
	if p, ok := c.providerAt(c.URL); ok {
		return p, true
	}
	return ps[0], true
}

// activeName is the provider in use, or "" when logged out.
func (c dunConfig) activeName() string {
	p, _ := c.active()
	return p.Name
}

// providerAt finds the provider whose URL is u.
func (c dunConfig) providerAt(u string) (providerConfig, bool) {
	for _, p := range c.providerList() {
		if strings.TrimRight(p.URL, "/") == strings.TrimRight(u, "/") {
			return p, true
		}
	}
	return providerConfig{}, false
}

// putProvider adds p, or replaces the provider of the same name.
func (c *dunConfig) putProvider(p providerConfig) {
	c.Providers = c.providerList()
	for i := range c.Providers {
		if c.Providers[i].Name == p.Name {
			c.Providers[i] = p
			return
		}
	}
	c.Providers = append(c.Providers, p)
}

// removeProvider drops the provider named name; it reports whether one was.
func (c *dunConfig) removeProvider(name string) bool {
	ps := c.providerList()
	for i, p := range ps {
		if p.Name == name {
			c.Providers = append(ps[:i:i], ps[i+1:]...)
			return true
		}
	}
	return false
}

// named finds the saved provider called name.
func (c dunConfig) named(name string) (providerConfig, bool) {
	for _, p := range c.providerList() {
		if p.Name == name {
			return p, true
		}
	}
	return providerConfig{}, false
}

// providerOp is one change to the logged-in providers, from /login, /logout
// or /model on any client.
type providerOp struct {
	Op    string `json:"op"` // login (replaces one of the same name), logout, use
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"` // login: "" or "openai", the only kind so far
	URL   string `json:"url,omitempty"`
	Key   string `json:"key,omitempty"`   // login: optional; again under a saved name, empty keeps the saved key
	Model string `json:"model,omitempty"` // use: an id, typed when the list is not there
}

// applyProviderOp changes the logged-in providers and says what it did. It
// is the one implementation: the TUI calls it directly (it must work when no
// engine can start), the engine calls it for the web and the phone. Keys
// never appear in the answer.
func applyProviderOp(op providerOp) (string, error) {
	cfg := loadConfig()
	name := strings.TrimSpace(op.Name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", errors.New("a provider name is one word")
	}
	var msg string
	switch op.Op {
	case "login":
		if op.Kind != "" && op.Kind != providerOpenAI {
			return "", errors.New("no provider kind " + op.Kind + " yet: " + providerOpenAI + " (any OpenAI-compatible endpoint) is the one there is")
		}
		u := strings.TrimRight(strings.TrimSpace(op.URL), "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return "", errors.New("the URL starts with http:// or https://")
		}
		p := providerConfig{Name: name, Kind: providerOpenAI, URL: u, Key: strings.TrimSpace(op.Key)}
		old, exists := cfg.named(name)
		if exists && p.Key == "" {
			p.Key = old.Key
		}
		_, loggedIn := cfg.active()
		wasDefault := exists && cfg.activeName() == name
		cfg.putProvider(p)
		switch {
		case wasDefault:
			cfg.Provider, cfg.URL, cfg.Key = p.Name, p.URL, p.Key
		case !loggedIn: // the first login is the default; the model comes next
			cfg.Provider, cfg.URL, cfg.Key, cfg.Model = p.Name, p.URL, p.Key, ""
		}
		msg = fmt.Sprintf("logged in to %s · %s · %s", name, u, keyNote(p.Key))
	case "logout":
		if _, ok := cfg.named(name); !ok {
			return "", errors.New("not logged in to " + name)
		}
		wasDefault := cfg.activeName() == name
		cfg.removeProvider(name)
		msg = "logged out of " + name
		switch {
		case len(cfg.Providers) == 0:
			cfg.Provider, cfg.URL, cfg.Key, cfg.Model = "", "", "", ""
			msg += "; no provider is left — /login to add one"
		case wasDefault:
			next := cfg.Providers[0]
			cfg.Provider, cfg.URL, cfg.Key, cfg.Model = next.Name, next.URL, next.Key, ""
			msg += "; the default provider is now " + next.Name + " — pick a model with /model"
		}
	case "use":
		p, ok := cfg.named(name)
		if !ok {
			return "", errors.New("not logged in to " + name)
		}
		model := strings.TrimSpace(op.Model)
		if model == "" || strings.ContainsAny(model, " \t") {
			return "", errors.New("a model id is one word")
		}
		cfg.use(p, model)
		msg = "default: " + model + " on " + name
	default:
		return "", errors.New("no provider operation " + op.Op + " (login, logout, use)")
	}
	if err := saveConfig(cfg); err != nil {
		return "", fmt.Errorf("failed to save config: %w", err)
	}
	return msg, nil
}

// What a first run is missing, said as the next step. A turn that failed on
// one of these is reported with the bare text (emitTurnError): the "agent:
// chat:" chain it arrives in is the code's path, not the user's.
var (
	errNoLogin = errors.New("not logged in — /login to connect to an AI service")
	errNoModel = errors.New("no model chosen — /model to pick one")
)

// setupMissing returns the setup error (errNoLogin, errNoModel) err wraps,
// or nil.
func setupMissing(err error) error {
	for _, e := range []error{errNoLogin, errNoModel} {
		if errors.Is(err, e) {
			return e
		}
	}
	return nil
}

// providerMissing says why the engine cannot talk to a model, or nil.
func providerMissing(url, model string) error {
	switch {
	case strings.TrimSpace(url) == "":
		return errNoLogin
	case strings.TrimSpace(model) == "":
		return errNoModel
	}
	return nil
}

// noProvider stands in for the LLM when providerMissing says why: every call
// fails with that, and nothing is contacted.
type noProvider struct{ why error }

func (n noProvider) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	return nil, n.why
}

// keyNote says whether there is a key, never what it is.
func keyNote(k string) string {
	if k == "" {
		return "no key"
	}
	return "key set"
}

// use makes p and model the saved defaults.
func (c *dunConfig) use(p providerConfig, model string) {
	c.putProvider(p)
	c.Provider, c.URL, c.Key, c.Model = p.Name, p.URL, p.Key, model
}

// providerLabel is what the ledger calls the provider at u: its saved name,
// else its host.
func providerLabel(fc dunConfig, u string) string {
	if p, ok := fc.providerAt(u); ok && p.Name != "" {
		return p.Name
	}
	return hostName(u)
}

func hostName(u string) string {
	if pu, err := neturl.Parse(u); err == nil && pu.Hostname() != "" {
		return pu.Hostname()
	}
	return u
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
	if err := os.WriteFile(configPath(), append(b, '\n'), 0o600); err != nil { // key is secret
		return err
	}
	// WriteFile's mode is for a new file only: one made before (by hand, or
	// by an older dun) keeps its mode. The login form says "saved
	// privately", so make it so.
	return os.Chmod(configPath(), 0o600)
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
	ids, _ := listModels(base, key)
	return ids
}

// listModels asks base for its models, at /v1/models and then /models (a
// base that already ends in /v1, as LiteLLM's often does, needs the second).
// With no ids it says why, from the last try: a status and the start of the
// body, or the network error — on a corporate network the reason (a 401, a
// proxy, a certificate) is the whole diagnosis.
func listModels(base, key string) ([]string, string) {
	base = strings.TrimRight(base, "/")
	client := &http.Client{Timeout: 6 * time.Second}
	why := "no model list"
	for _, u := range []string{base + "/v1/models", base + "/models"} {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			why = err.Error()
			continue
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			why = err.Error()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 160))
			resp.Body.Close()
			why = fmt.Sprintf("%s: HTTP %d %s", u, resp.StatusCode, strings.Join(strings.Fields(string(b)), " "))
			continue
		}
		ids := decodeModels(resp)
		if len(ids) > 0 {
			return ids, ""
		}
		why = u + ": an empty model list (the key may have no models)"
	}
	return nil, why
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
//
// usageFor(model) is set on each client as it is built, before anyone can use
// it — so a sub-agent's spend reaches the same meter as the parent's, and the
// ledger line names the model it ran on (dun/spend.go, dun/ledger.go).
func clientCache(url, key string, usageFor func(model string) func(llm.Usage)) func(string) (agent.LLMRunner, error) {
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
		c.OnUsage = usageFor(model)
		cache[model] = c
		return c, nil
	}
}

// parseRetryBudgets interprets the persisted retry budgets: request scope (a
// negative value means unbounded) and turn scope ("0" disables the turn-scope
// retry, same as DUN_TURN_RETRY_BUDGET). Empty = unset. Junk is ignored with a
// warning; an env var still wins over both, because applyRetryPolicy and
// turnRetryPolicy only override when set.
func parseRetryBudgets(fc dunConfig) (request, turn time.Duration, requestSet, turnSet bool) {
	if v := fc.RetryBudget; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dun: ignoring invalid retry_budget %q in config\n", v)
		} else {
			request, requestSet = d, true
		}
	}
	if v := fc.TurnRetryBudget; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			fmt.Fprintf(os.Stderr, "dun: ignoring invalid turn_retry_budget %q in config\n", v)
		} else {
			turn, turnSet = d, true
		}
	}
	return request, turn, requestSet, turnSet
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
