package dun

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Capability reports, from raglit (4c464a0, 3f1895e).
//
// USER 2026-10-24: raglit "as pleasant to install as poly-lsp": useful with
// nothing configured (keyword search), and each optional model role with a
// note of what it adds and what it needs. raglit sends notifications/message
// from logger "raglit/setup" once per session after initialized, level
// "warning" only when a core capability is off:
//
//	{"capabilities": [{name, on, via (on), needs, how (off)}],
//	 "roles": [{role, provider, model, set, hasKey, adds, fallsBackTo}],
//	 "extractors": [{name, found, path, adds, optional, install: {cmd, needs}}],
//	 "extractorsSeenBy": "daemon (pid N)", "missing": N}
//
// `raglit doctor --json` carries the same under its own keys
// (ParseRaglitDoctor). It is a SetupReport like poly-lsp's, with these
// fields instead of languages: one notification handler, one place on
// ServerState, one table per server's slash command.
//
// What dun does with it: one quiet line per session, only when a core
// capability a model from the person's own providers turns on is off
// (semantic search, scanned pages); /raglit has the table; /mcp info says
// "semantic search off"; /raglit setup (cmd/dun/ragsetup.go) picks the
// models. Extractors are never said in the conversation: installing poppler
// is a system package, not a choice in a chat, and doctor lists them.

// SetupCapability is one thing the server can do, whether it is on, what
// provides it, or what would turn it on and how.
type SetupCapability struct {
	Name  string `json:"name"`
	On    bool   `json:"on"`
	Via   string `json:"via,omitempty"`
	Needs string `json:"needs,omitempty"`
	How   string `json:"how,omitempty"`
}

// SetupRole is one model role as the server has it configured.
type SetupRole struct {
	Role        string `json:"role"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Set         bool   `json:"set"`
	HasKey      bool   `json:"hasKey"`
	Adds        string `json:"adds"`
	FallsBackTo string `json:"fallsBackTo,omitempty"` // the role whose model runs it when unset
}

// SetupExtractor is one external program a file kind needs.
type SetupExtractor struct {
	Name     string        `json:"name"`
	Found    bool          `json:"found"`
	Path     string        `json:"path,omitempty"`
	Adds     string        `json:"adds"`
	Optional bool          `json:"optional"`
	Install  *SetupInstall `json:"install,omitempty"`
}

// Capability names raglit reports, which dun says by name. A contract with
// raglit (cmd/raglit/doctorreport.go); a renamed one is listed in the
// table and left out of the line.
const (
	CapSemantic = "semantic and hybrid search"
	CapScans    = "scanned pages and photos"
)

// ragLineCaps are the capabilities the conversation's line may name: core
// ones (raglit warns when they are off) that a model from the person's own
// providers turns on — cheap to fix with what they have. Keyword search is
// always on; PDF text needs poppler, which doctor names.
var ragLineCaps = []struct{ name, say, model string }{
	{CapSemantic, "semantic search", "an embedding model"},
	{CapScans, "scanned pages and photos", "a vision model"},
}

// ragSetRoles is raglit's "how" for a capability a model role turns on; dun
// says its own command instead.
const ragSetRoles = "raglit init, or PUT /api/settings/models on the daemon"

// CapabilityOff reports whether the report lists name, off.
func (r SetupReport) CapabilityOff(name string) bool {
	for _, c := range r.Capabilities {
		if c.Name == name {
			return !c.On
		}
	}
	return false
}

// CapabilityLine is the one quiet line for a capability report, "" when
// nothing in ragLineCaps is off.
func CapabilityLine(r SetupReport, slash string) string {
	var off, models []string
	for _, c := range ragLineCaps {
		if r.CapabilityOff(c.name) {
			off = append(off, c.say)
			models = append(models, c.model)
		}
	}
	switch len(off) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s: %s is off — /%s setup picks %s from your providers", slash, off[0], slash, models[0])
	}
	return fmt.Sprintf("%s: %s are off — /%s setup picks the models from your providers", slash, joinAnd(off), slash)
}

// RagRoleName is a role as the table and the setup say it: raglit's
// "identity" writes the captions and summaries.
func RagRoleName(role string) string {
	if role == "identity" {
		return "summaries"
	}
	return role
}

// RagHow is a capability's "how" in dun's words: raglit's own way to set a
// model role becomes /<slash> setup.
func RagHow(how, slash string) string {
	return strings.ReplaceAll(how, ragSetRoles, "/"+slash+" setup")
}

// CapabilityTable is the report as rows, for bare /raglit: each capability
// on (with what provides it) or off (with what it needs and how), each role
// with its model or what it falls back to, and each extractor found or
// missing with its install command.
func CapabilityTable(r SetupReport, slash string) string {
	rows := []string{"what it can do here:"}
	for _, c := range r.Capabilities {
		if c.On {
			row := "  on   " + c.Name
			if c.Via != "" {
				row += " · " + c.Via
			}
			rows = append(rows, row)
			continue
		}
		row := "  off  " + c.Name
		if c.Needs != "" {
			row += " · needs " + c.Needs
		}
		if c.How != "" {
			row += " · " + RagHow(c.How, slash)
		}
		rows = append(rows, row)
	}
	if len(r.Roles) > 0 {
		rows = append(rows, "models, by role:")
		w := 0
		for _, ro := range r.Roles {
			w = max(w, len(RagRoleName(ro.Role)))
		}
		for _, ro := range r.Roles {
			row := fmt.Sprintf("  %-*s  ", w, RagRoleName(ro.Role))
			switch {
			case ro.Set:
				row += strings.TrimSpace(ro.Provider + " " + ro.Model)
				if ro.Provider != "" && !ro.HasKey {
					row += " (no key)"
				}
			case ro.FallsBackTo != "":
				row += "none → " + RagRoleName(ro.FallsBackTo) + "'s model"
			default:
				row += "none"
			}
			if ro.Adds != "" {
				row += " · " + ro.Adds
			}
			rows = append(rows, row)
		}
	}
	if len(r.Extractors) > 0 {
		head := "programs it reads files with:"
		if r.ExtractorsSeenBy != "" {
			head = "programs it reads files with (as the " + r.ExtractorsSeenBy + " sees them):"
		}
		rows = append(rows, head)
		for _, e := range r.Extractors {
			row := "  " + e.Name
			switch {
			case e.Found:
				row += " found"
			case e.Optional:
				row += " missing (optional)"
			default:
				row += " missing"
			}
			if !e.Found {
				if e.Adds != "" {
					row += " · adds " + e.Adds
				}
				if e.Install != nil {
					row += " · " + e.Install.Cmd
				}
			}
			rows = append(rows, row)
		}
	}
	return strings.Join(append(rows, "/"+slash+" setup picks a model for each role from your providers; again, with none, to undo"), "\n")
}

// RaglitDoctor is `raglit doctor --json`: what raglit can do as the daemon
// (or, without one, this shell) sees it, and its update block.
type RaglitDoctor struct {
	Binary     string `json:"binary"`
	Build      string `json:"build"`
	Platform   string `json:"platform"`
	Home       string `json:"home"`       // the config home raglit read: <home>/config.json
	Configured bool   `json:"configured"` // a config.json exists
	Daemon     string `json:"daemon"`     // the daemon's URL when it answered, "" when not
	SetupReport
	Update json.RawMessage `json:"update,omitempty"` // poly-lsp's update shape; cmd/dun reads it
}

// ParseRaglitDoctor reads `raglit doctor --json`, ok=false when out is not
// that (an older raglit prints its text report, or an error).
func ParseRaglitDoctor(out []byte) (RaglitDoctor, bool) {
	var d RaglitDoctor
	if json.Unmarshal(out, &d) != nil || d.Capabilities == nil {
		return RaglitDoctor{}, false
	}
	return d, true
}
