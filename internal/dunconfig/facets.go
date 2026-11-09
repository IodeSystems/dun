package dunconfig

import (
	"fmt"
	"sort"
	"strings"
)

// facets.go: the one resolver for every model role (FacetNames). The
// session (cmd/dun), dund's retrieval (ragd.Resolve), /model and dun doctor
// all ask it. Precedence, field by field: CLI flag > env > facets.<role> >
// the old top-level fields (model; vision_model and vision_provider) > the
// role's default. The config file is read, never rewritten here.

// Where a role's model came from.
const (
	SourceFlag    = "flag"
	SourceEnv     = "env"
	SourceConfig  = "config"            // facets.<role>
	SourceOld     = "config, old field" // model, vision_model
	SourceDefault = "default"
	SourceSession = "session" // a /model pick for this session only (the TUI's)
)

// Pick is a role as one layer names it. Any part may be empty: a layer
// that names only a model keeps the endpoint of the layer below. Provider
// is a saved provider's name; URL an address (a flag or env var).
type Pick struct {
	Provider, URL, Key, Model, Effort string
}

// Overrides are the CLI flags given, by role.
type Overrides map[string]Pick

// Resolved is one role as it is served.
type Resolved struct {
	Facet string
	// Provider is the saved provider's name; for an address that is not
	// saved, its host. Empty for child with no provider named: main's.
	Provider string
	URL, Key string
	Model    string // empty: not set (child: main's model)
	Effort   string // empty: the saved /effort level
	Source   string // the layer Model came from
	// Off is why a named model cannot be served (its provider is not saved,
	// none is logged in); empty when it can, or when no model is named.
	Off string
}

// On says the role has a model it can be served on.
func (r Resolved) On() bool { return r.Model != "" && r.Off == "" }

// Line is the role in one line, as /model and dun doctor show it: the name,
// the model on its provider (or why not), and where it came from. The key
// is never in it.
func (r Resolved) Line() string {
	var what string
	switch {
	case r.Model == "" && r.Facet == FacetChild && r.Off == "":
		what = "same as main"
		if r.Provider != "" {
			what = "provider " + r.Provider + " with no model: a spawn names one"
		}
	case r.Model == "" && r.Facet == FacetMain:
		what = "no model chosen"
	case r.Model == "":
		what = "off"
		if r.Off != "" {
			what += " (" + r.Off + ")"
		}
	default:
		on := r.Provider
		if on == "" {
			on = "main's provider"
		}
		what = r.Model + " on " + on
		if r.Effort != "" {
			what += ", effort " + r.Effort
		}
		if r.Off != "" {
			what += ": off (" + r.Off + ")"
		}
	}
	return fmt.Sprintf("%-9s %s · %s", r.Facet, what, r.Source)
}

// envPick is what the environment says of name. Only main has env vars.
func envPick(name string, env func(string) string) Pick {
	if env == nil || name != FacetMain {
		return Pick{}
	}
	return Pick{URL: env("DUN_URL"), Key: env("DUN_LLM_KEY"), Model: env("DUN_MODEL")}
}

// oldPick is the top-level field a role had before facets.
func (c DunConfig) oldPick(name string) Pick {
	switch name {
	case FacetMain:
		return Pick{Model: c.Model}
	case FacetVision:
		return Pick{Provider: c.VisionProvider, Model: c.VisionModel}
	}
	return Pick{}
}

// defaultPick is the role when nothing names it.
func defaultPick(name string) Pick {
	if name == FacetEmbed {
		return Pick{Model: DefaultEmbedModel}
	}
	return Pick{}
}

// Resolve is role name as flags, env and the config give it.
func (c DunConfig) Resolve(name string, flags Overrides, env func(string) string) Resolved {
	f := c.Facets[name]
	layers := []struct {
		src string
		p   Pick
	}{
		{SourceFlag, flags[name]},
		{SourceEnv, envPick(name, env)},
		{SourceConfig, Pick{Provider: f.Provider, Model: f.Model, Effort: f.Effort}},
		{SourceOld, c.oldPick(name)},
		{SourceDefault, defaultPick(name)},
	}
	r := Resolved{Facet: name, Source: SourceDefault}
	var where Pick // the first layer that names an endpoint
	key := ""
	for _, l := range layers {
		p := l.p
		if r.Model == "" && strings.TrimSpace(p.Model) != "" {
			r.Model, r.Source = strings.TrimSpace(p.Model), l.src
		}
		if r.Effort == "" {
			r.Effort = strings.TrimSpace(p.Effort)
		}
		if where == (Pick{}) && (p.Provider != "" || p.URL != "") {
			where = Pick{Provider: p.Provider, URL: p.URL}
		}
		if key == "" {
			key = p.Key
		}
	}
	switch {
	case where.Provider != "":
		p, ok := c.Named(where.Provider)
		r.Provider = where.Provider
		if !ok {
			r.Off = "no saved provider " + where.Provider
			return r
		}
		r.URL, r.Key = p.URL, p.Key
	case where.URL != "":
		r.URL = strings.TrimRight(where.URL, "/")
		r.Provider = HostName(r.URL)
		if p, ok := c.ProviderAt(r.URL); ok {
			r.Provider, r.Key = p.Name, p.Key
		}
	case name == FacetChild:
		// No provider named: the session's own, whichever that is.
	default:
		p, ok := c.Active()
		if !ok {
			if r.Model != "" {
				r.Off = "no provider is logged in"
			}
			return r
		}
		r.Provider, r.URL, r.Key = p.Name, p.URL, p.Key
	}
	if key != "" {
		r.Key = key
	}
	return r
}

// ResolveAll is every role, in FacetNames order.
func (c DunConfig) ResolveAll(flags Overrides, env func(string) string) []Resolved {
	out := make([]Resolved, len(FacetNames))
	for i, n := range FacetNames {
		out[i] = c.Resolve(n, flags, env)
	}
	return out
}

// UnknownFacets are the names in facets that are no role, sorted.
func (c DunConfig) UnknownFacets() []string {
	var out []string
	for n := range c.Facets {
		if !IsFacet(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// SetFacet sets role name; an empty Facet removes it.
func (c *DunConfig) SetFacet(name string, f Facet) {
	if f == (Facet{}) {
		delete(c.Facets, name)
		if len(c.Facets) == 0 {
			c.Facets = nil
		}
		return
	}
	if c.Facets == nil {
		c.Facets = map[string]Facet{}
	}
	c.Facets[name] = f
}

// ParsePick reads "<provider>/<model>" or "<model>": the part before the
// first slash is a provider when one of that name is saved, else the whole
// string is a model id on the role's default provider (an OpenRouter id
// such as anthropic/claude-x has a slash of its own).
func (c DunConfig) ParsePick(s string) (provider, model string) {
	s = strings.TrimSpace(s)
	if p, m, ok := strings.Cut(s, "/"); ok && m != "" {
		if _, saved := c.Named(p); saved {
			return p, m
		}
	}
	return "", s
}
