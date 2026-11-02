// Package dunconfig is dun's saved settings and where they live.
//
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
package dunconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	shim "github.com/iodesystems/openai-claude-api-shim"
)

func DundLog() string { return DunHome() + "/dund.log" }

// ProviderOpenAI is the one kind of provider today: any OpenAI-compatible
// endpoint (/v1/models, /v1/chat/completions), with an optional bearer key.
// Specific providers (OpenRouter, Anthropic, Azure…) come later as kinds.
const ProviderOpenAI = "openai"

type DunConfig struct {
	URL   string `json:"url,omitempty"`
	Model string `json:"model,omitempty"`
	Key   string `json:"key,omitempty"`
	// Providers are the endpoints /model lists models from, each with its own
	// key. Provider names the one in use. URL and Key above stay the active
	// provider's, written on every switch, so a config with no providers (or
	// a binary that predates them) still means what it says.
	Provider  string           `json:"provider,omitempty"`
	Providers []ProviderConfig `json:"providers,omitempty"`
	// Provider-retry budgets, persisted so an endpoint that needs them does not
	// need the env vars on every launch. Precedence: CLI flag > env
	// (DUN_RETRY_BUDGET / DUN_TURN_RETRY_BUDGET) > these fields > the built-in
	// defaults. Empty = unset; a negative value means unbounded (request scope).
	RetryBudget     string `json:"retry_budget,omitempty"`
	TurnRetryBudget string `json:"turn_retry_budget,omitempty"` // "0" disables turn-scope retry
	// Retry5xxAttempts is how many times a 5xx is retried (/retry attempts);
	// 0 is agentkit's default (5). DUN_RETRY_5XX_ATTEMPTS wins over it.
	Retry5xxAttempts int `json:"retry_5xx_attempts,omitempty"`
	// SpendBudget is the monthly budget, in USD, that dun spend counts down.
	// Anthropic has no balance to read, so it is a figure the user sets; 0 is none.
	SpendBudget float64 `json:"spend_budget_usd,omitempty"`
	// SpendKnown is the last real figure the user read off the Console.
	SpendKnown SpendKnown `json:"spend_known,omitzero"`
	// Loops is /loops: how far dun's loop detection goes (loopscmd.go).
	Loops LoopConfig `json:"loops,omitzero"`
	// VisionModel describes images for a model that cannot read them (a model
	// id, on the active provider unless VisionProvider names another
	// logged-in one). Empty: a message with images to such a model is
	// refused. See images.go.
	VisionModel    string `json:"vision_model,omitempty"`
	VisionProvider string `json:"vision_provider,omitempty"`
	// NoUpdates is /upgrade off: a release build does not check for new
	// releases (release.go). Other builds never do.
	NoUpdates bool `json:"no_updates,omitempty"`
	// Reasoning is providerConfig.Reasoning for a config with no providers.
	Reasoning string `json:"reasoning,omitempty"`
}

// ProviderConfig is one OpenAI-compatible endpoint: /v1/models and
// /v1/chat/completions under URL, authorized by Key.
type ProviderConfig struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // "" is providerOpenAI
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
	// Reasoning is how the endpoint is asked to think (/effort): "" detects
	// it from the address and model; openai (reasoning_effort, what a LiteLLM
	// proxy maps per provider), openrouter, llamacpp or anthropic name it.
	Reasoning string `json:"reasoning,omitempty"`
}

// ProviderList returns the logged-in providers. A config from before
// providers that names a URL has one, the top-level URL and Key, named after
// its host. A config with neither has none: logged out.
func (c DunConfig) ProviderList() []ProviderConfig {
	if len(c.Providers) > 0 {
		return c.Providers
	}
	if c.URL == "" {
		return nil
	}
	return []ProviderConfig{{Name: HostName(c.URL), Kind: ProviderOpenAI, URL: c.URL, Key: c.Key}}
}

// Active returns the provider in use: the one Provider names, else the one
// at the top-level URL, else the first; false when logged out.
func (c DunConfig) Active() (ProviderConfig, bool) {
	ps := c.ProviderList()
	if len(ps) == 0 {
		return ProviderConfig{}, false
	}
	for _, p := range ps {
		if p.Name == c.Provider {
			return p, true
		}
	}
	if p, ok := c.ProviderAt(c.URL); ok {
		return p, true
	}
	return ps[0], true
}

// ActiveName is the provider in use, or "" when logged out.
func (c DunConfig) ActiveName() string {
	p, _ := c.Active()
	return p.Name
}

// ProviderAt finds the provider whose URL is u.
func (c DunConfig) ProviderAt(u string) (ProviderConfig, bool) {
	for _, p := range c.ProviderList() {
		if strings.TrimRight(p.URL, "/") == strings.TrimRight(u, "/") {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// PutProvider adds p, or replaces the provider of the same name.
func (c *DunConfig) PutProvider(p ProviderConfig) {
	c.Providers = c.ProviderList()
	for i := range c.Providers {
		if c.Providers[i].Name == p.Name {
			c.Providers[i] = p
			return
		}
	}
	c.Providers = append(c.Providers, p)
}

// RemoveProvider drops the provider named name; it reports whether one was.
func (c *DunConfig) RemoveProvider(name string) bool {
	ps := c.ProviderList()
	for i, p := range ps {
		if p.Name == name {
			c.Providers = append(ps[:i:i], ps[i+1:]...)
			return true
		}
	}
	return false
}

// Named finds the saved provider called name.
func (c DunConfig) Named(name string) (ProviderConfig, bool) {
	for _, p := range c.ProviderList() {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// FreeName is a name for a new login at u: base, unless a saved provider
// has it for another address, then base-2, base-3… — a second local server
// is not saved over the first. A login again to the same address keeps the
// name (and replaces that one).
func (c DunConfig) FreeName(base, u string) string {
	if base == "" {
		return ""
	}
	same := func(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }
	for i := 1; ; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s-%d", base, i)
		}
		if p, ok := c.Named(n); !ok || same(p.URL, u) {
			return n
		}
	}
}

// ProviderOp is one change to the logged-in providers, from /login, /logout
// or /model on any client.
type ProviderOp struct {
	Op    string `json:"op"` // login (replaces one of the same name), logout, use
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"` // login: "" or "openai", the only kind so far
	URL   string `json:"url,omitempty"`
	Key   string `json:"key,omitempty"`   // login: optional; again under a saved name, empty keeps the saved key
	Model string `json:"model,omitempty"` // use: an id, typed when the list is not there
	// NewName is rename's new name for Name (the login form fills a name in
	// from the address; this is how it is changed).
	NewName string `json:"new_name,omitempty"`
}

// ApplyProviderOp changes the logged-in providers and says what it did. It
// is the one implementation: the TUI calls it directly (it must work when no
// engine can start), the engine calls it for the web and the phone. Keys
// never appear in the answer.
func ApplyProviderOp(op ProviderOp) (string, error) {
	cfg := LoadConfig()
	name := strings.TrimSpace(op.Name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", errors.New("a provider name is one word")
	}
	var msg string
	switch op.Op {
	case "login":
		if op.Kind != "" && op.Kind != ProviderOpenAI {
			return "", errors.New("no provider kind " + op.Kind + " yet: " + ProviderOpenAI + " (any OpenAI-compatible endpoint) is the one there is")
		}
		u := strings.TrimRight(strings.TrimSpace(op.URL), "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return "", errors.New("the URL starts with http:// or https://")
		}
		p := ProviderConfig{Name: name, Kind: ProviderOpenAI, URL: u, Key: strings.TrimSpace(op.Key)}
		old, exists := cfg.Named(name)
		if exists && p.Key == "" {
			p.Key = old.Key
		}
		_, loggedIn := cfg.Active()
		wasDefault := exists && cfg.ActiveName() == name
		cfg.PutProvider(p)
		switch {
		case wasDefault:
			cfg.Provider, cfg.URL, cfg.Key = p.Name, p.URL, p.Key
		case !loggedIn: // the first login is the default; the model comes next
			cfg.Provider, cfg.URL, cfg.Key, cfg.Model = p.Name, p.URL, p.Key, ""
		}
		msg = fmt.Sprintf("logged in to %s · %s · %s", name, u, KeyNote(p.Key))
	case "logout":
		if _, ok := cfg.Named(name); !ok {
			return "", errors.New("not logged in to " + name)
		}
		wasDefault := cfg.ActiveName() == name
		cfg.RemoveProvider(name)
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
		p, ok := cfg.Named(name)
		if !ok {
			return "", errors.New("not logged in to " + name)
		}
		model := strings.TrimSpace(op.Model)
		if model == "" || strings.ContainsAny(model, " \t") {
			return "", errors.New("a model id is one word")
		}
		cfg.Use(p, model)
		msg = "default: " + model + " on " + name
	case "rename":
		p, ok := cfg.Named(name)
		if !ok {
			return "", errors.New("not logged in to " + name)
		}
		to := strings.TrimSpace(op.NewName)
		if to == "" || strings.ContainsAny(to, " \t") {
			return "", errors.New("a provider name is one word")
		}
		if _, taken := cfg.Named(to); taken {
			return "", errors.New(to + " is taken — /logout " + to + " first")
		}
		wasDefault := cfg.ActiveName() == name
		cfg.Providers = cfg.ProviderList()
		for i := range cfg.Providers {
			if cfg.Providers[i].Name == p.Name {
				cfg.Providers[i].Name = to // in place: the list keeps its order
			}
		}
		if wasDefault {
			cfg.Provider = to
		}
		msg = "renamed " + name + " to " + to
	default:
		return "", errors.New("no provider operation " + op.Op + " (login, logout, use, rename)")
	}
	if err := SaveConfig(cfg); err != nil {
		return "", fmt.Errorf("failed to save config: %w", err)
	}
	return msg, nil
}

// What a first run is missing, said as the next step. A turn that failed on
// one of these is reported with the bare text (emitTurnError): the "agent:
// chat:" chain it arrives in is the code's path, not the user's.
var (
	ErrNoLogin = errors.New("not logged in — /login to connect to an AI service")
	ErrNoModel = errors.New("no model chosen — /model to pick one")
)

// SetupMissing returns the setup error (errNoLogin, errNoModel) err wraps,
// or nil.
func SetupMissing(err error) error {
	for _, e := range []error{ErrNoLogin, ErrNoModel} {
		if errors.Is(err, e) {
			return e
		}
	}
	return nil
}

// ProviderMissing says why the engine cannot talk to a model, or nil.
func ProviderMissing(url, model string) error {
	switch {
	case strings.TrimSpace(url) == "":
		return ErrNoLogin
	case strings.TrimSpace(model) == "":
		return ErrNoModel
	}
	return nil
}

// KeyNote says whether there is a key, never what it is.
func KeyNote(k string) string {
	if k == "" {
		return "no key"
	}
	return "key set"
}

// Use makes p and model the saved defaults.
func (c *DunConfig) Use(p ProviderConfig, model string) {
	c.PutProvider(p)
	c.Provider, c.URL, c.Key, c.Model = p.Name, p.URL, p.Key, model
}

func HostName(u string) string {
	if pu, err := neturl.Parse(u); err == nil && pu.Hostname() != "" {
		return pu.Hostname()
	}
	return u
}

func DunHome() string {
	if h := os.Getenv("DUN_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dun"
	}
	return filepath.Join(home, ".dun")
}

func ConfigPath() string { return filepath.Join(DunHome(), "config.json") }

// LoadConfig reads the saved config; a missing/corrupt file yields a zero config.
func LoadConfig() DunConfig {
	var c DunConfig
	if b, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func SaveConfig(c DunConfig) error {
	if err := os.MkdirAll(DunHome(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(ConfigPath(), append(b, '\n'), 0o600); err != nil { // key is secret
		return err
	}
	// WriteFile's mode is for a new file only: one made before (by hand, or
	// by an older dun) keeps its mode. The login form says "saved
	// privately", so make it so.
	return os.Chmod(ConfigPath(), 0o600)
}

func FirstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func MaskKey(k string) string {
	if k == "" {
		return "(none)"
	}
	if len(k) <= 4 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}

// FetchModels queries an OpenAI-compatible /models endpoint (best-effort).
func FetchModels(base, key string) []string {
	ids, _ := ListModels(base, key)
	return ids
}

// ListModels asks base for its models, at /v1/models and then /models (a
// base that already ends in /v1, as LiteLLM's often does, needs the second).
// With no ids it says why, from the last try: a status and the start of the
// body, or the network error — on a corporate network the reason (a 401, a
// proxy, a certificate) is the whole diagnosis.
func ListModels(base, key string) ([]string, string) {
	ids, f := AskModels(&http.Client{Timeout: 6 * time.Second}, base, key)
	return ids, f.Why
}

// ModelsFail is why a service gave no model list. refused: it answered
// 401 or 403 (the key). unreachable: no try got an answer at all (nothing
// listening, no such host, a timeout).
type ModelsFail struct {
	Why         string
	Refused     bool
	Unreachable bool
}

// AskModels is listModels with the reason in parts, on client (the login's
// check waits longer than a probe of a local server does).
func AskModels(client *http.Client, base, key string) ([]string, ModelsFail) {
	base = strings.TrimRight(base, "/")
	f := ModelsFail{Why: "no model list", Unreachable: true}
	for _, u := range []string{base + "/v1/models", base + "/models"} {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			f.Why = err.Error()
			continue
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			// Anthropic's chat takes the bearer key (its OpenAI-compatible
			// endpoint); its model list is its own API's, which asks for the
			// key and a version in headers of its own.
			if req.URL.Hostname() == "api.anthropic.com" {
				req.Header.Set("x-api-key", key)
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			f.Why = err.Error()
			continue
		}
		f.Unreachable = false
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 160))
			resp.Body.Close()
			f.Why = fmt.Sprintf("%s: HTTP %d %s", u, resp.StatusCode, strings.Join(strings.Fields(string(b)), " "))
			f.Refused = f.Refused || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
			continue
		}
		ids := decodeModels(resp)
		if len(ids) > 0 {
			return ids, ModelsFail{}
		}
		f.Why = u + ": an empty model list (the key may have no models)"
	}
	return nil, f
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

// PricesPath is $DUN_HOME/prices.json: dun's own override of the price list its
// Claude cost estimates come from, in USD per million tokens, by model or model
// prefix:
//
//	{"claude-opus-5-5": {"input": 4, "write_5m": 5, "write_1h": 8, "read": 0.2, "output": 20}}
//
// It wins over the list shipped in openai-claude-api-shim and is read at start.
// It is dun's, not shared: corrallm has its own, because either may be installed
// without the other.
func PricesPath() string { return filepath.Join(DunHome(), "prices.json") }

// LoadPrices reads it. No file is no override; a bad one is logged and ignored,
// because a cost estimate is no reason not to start.
func LoadPrices() shim.Prices {
	b, err := os.ReadFile(PricesPath())
	if err != nil {
		return nil
	}
	var p shim.Prices
	if err := json.Unmarshal(b, &p); err != nil {
		log.Printf("dun: ignoring %s: %v", PricesPath(), err)
		return nil
	}
	return p
}

// SpendKnown is a real figure and when it was true.
type SpendKnown struct {
	USD float64   `json:"usd"`
	At  time.Time `json:"at"`
}
