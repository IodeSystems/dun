package ragd

import (
	"fmt"
	"strings"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// Roles is the facets resolved to endpoints, and what each one is.
type Roles struct {
	Models raglit.Models
	// Said is one line per role: its model and provider, or why it is off.
	Said []string
}

// Resolve turns the config's facets into raglit's model roles. A facet names
// a saved provider (its URL and key are used) or none (the active one). With
// no embed facet, embed is DefaultEmbedModel on the active provider. A role
// that is absent, or names a provider that is not saved, is off.
func Resolve(cfg dunconfig.DunConfig) Roles {
	var r Roles
	role := func(name string, dst *raglit.ModelRole, off string) {
		f, ok := cfg.Facets[name]
		if !ok && name == dunconfig.FacetEmbed {
			f, ok = dunconfig.Facet{Model: dunconfig.DefaultEmbedModel}, true
		}
		if !ok || strings.TrimSpace(f.Model) == "" {
			r.Said = append(r.Said, name+": off ("+off+")")
			return
		}
		var p dunconfig.ProviderConfig
		var found bool
		if f.Provider == "" {
			p, found = cfg.Active()
		} else {
			p, found = cfg.Named(f.Provider)
		}
		if !found {
			who := "no provider is logged in"
			if f.Provider != "" {
				who = "no saved provider " + f.Provider
			}
			r.Said = append(r.Said, fmt.Sprintf("%s: off (%s; %s)", name, who, off))
			return
		}
		*dst = raglit.ModelRole{BaseURL: p.URL, APIKey: p.Key, Model: f.Model}
		r.Said = append(r.Said, fmt.Sprintf("%s: %s on %s", name, f.Model, p.Name))
	}
	role(dunconfig.FacetEmbed, &r.Models.Embed, "search by text only")
	role(dunconfig.FacetVision, &r.Models.Vision, "no OCR by a model; tesseract if it is installed")
	role(dunconfig.FacetSegment, &r.Models.Segment, "the vision model segments, else rules")
	role(dunconfig.FacetIdentity, &r.Models.Identity, "no captions or tags from a model")
	for name := range cfg.Facets {
		switch name {
		case dunconfig.FacetEmbed, dunconfig.FacetVision, dunconfig.FacetSegment, dunconfig.FacetIdentity:
		default:
			r.Said = append(r.Said, name+": not a role dund knows; ignored")
		}
	}
	return r
}
