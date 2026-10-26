package dun

// Known MCP servers /mcp add offers to fill in. Suggestions only: nothing
// here is added until the person submits the form.

// ServerPreset is one known server: a command line or a URL.
type ServerPreset struct {
	ID, Description string
	Command         string
	Args            []string
	URL             string
}

// ServerPresets are the servers /mcp add suggests.
var ServerPresets = []ServerPreset{
	{ID: "playwright", Description: "browser automation (Playwright), headless and isolated",
		Command: "npx", Args: []string{"@playwright/mcp@latest", "--headless", "--isolated"}},
	{ID: "chrome-devtools", Description: "drive and inspect Chrome through its DevTools protocol",
		Command: "npx", Args: []string{"chrome-devtools-mcp@latest"}},
	{ID: "mobile", Description: "iOS and Android devices and simulators (mobile-mcp)",
		Command: "npx", Args: []string{"@mobilenext/mobile-mcp@latest"}},
	// UNVERIFIED: the Figma desktop app's local MCP server, as Figma's docs
	// describe it — not checked against a running app here.
	{ID: "figma", Description: "the Figma desktop app's local server (Dev Mode MCP; the app must be running)",
		URL: "http://127.0.0.1:3845/mcp"},
}

// Preset finds a preset by id.
func Preset(id string) (ServerPreset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return ServerPreset{}, false
}
