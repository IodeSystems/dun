package dundcli

import (
	"os"
	"path/filepath"
)

// defaultWebRoot is where `make web` installs the client (WEB_ROOT in the
// Makefile): $XDG_DATA_HOME/dun/web, else ~/.local/share/dun/web.
func defaultWebRoot() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(h, ".local", "share")
	}
	return filepath.Join(base, "dun", "web")
}

// resolveWebRoot is the web root serve and check both use: the --web-root
// flag when given; else, in a build with no embedded client, the installed
// copy when it has index.html; else "" (the embedded client, or the
// no-client page).
func resolveWebRoot(flag string, embedded bool) string {
	if flag != "" || embedded {
		return flag
	}
	d := defaultWebRoot()
	if d == "" {
		return ""
	}
	if st, err := os.Stat(filepath.Join(d, "index.html")); err != nil || st.IsDir() {
		return ""
	}
	return d
}
