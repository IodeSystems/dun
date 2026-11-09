package ragd

import (
	"fmt"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// Roles is the facets resolved to endpoints, and what each one is.
type Roles struct {
	Models raglit.Models
	// Said is one line per role: its model and provider, or why it is off.
	Said []string
	// CapsDir is the capability store (<home>/capabilities), where the role
	// probes record what they measured; dund reads the embed dimension
	// from it. Empty: not read.
	CapsDir string
}

// Resolve turns the config's retrieval roles into raglit's model roles, by
// dunconfig's resolver (the one the session's models use): a facet names a
// saved provider (its URL and key are used) or none (the active one). With
// no embed facet, embed is DefaultEmbedModel on the active provider; with no
// vision facet, vision is the old vision_model. A role that is absent, or
// names a provider that is not saved, is off.
func Resolve(cfg dunconfig.DunConfig) Roles {
	var r Roles
	role := func(name string, dst *raglit.ModelRole, off string) {
		f := cfg.Resolve(name, nil, nil)
		switch {
		case f.On():
			*dst = raglit.ModelRole{BaseURL: f.URL, APIKey: f.Key, Model: f.Model}
			r.Said = append(r.Said, fmt.Sprintf("%s: %s on %s", name, f.Model, f.Provider))
		case f.Off != "":
			r.Said = append(r.Said, fmt.Sprintf("%s: off (%s; %s)", name, f.Off, off))
		default:
			r.Said = append(r.Said, name+": off ("+off+")")
		}
	}
	role(dunconfig.FacetEmbed, &r.Models.Embed, "search by text only")
	role(dunconfig.FacetVision, &r.Models.Vision, "no OCR by a model; tesseract if it is installed")
	role(dunconfig.FacetSegment, &r.Models.Segment, "the vision model segments, else rules")
	role(dunconfig.FacetIdentity, &r.Models.Identity, "no captions or tags from a model")
	for _, name := range cfg.UnknownFacets() {
		r.Said = append(r.Said, name+": not a role dund knows; ignored")
	}
	return r
}
