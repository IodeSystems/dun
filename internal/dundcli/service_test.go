package dundcli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	u := renderUnit("/home/me/go/bin/dun", []string{"dund"}, "/home/me/.dun", []string{"--http", "192.168.1.76:20003"}, []string{"X=a b"})
	for _, want := range []string{
		"ExecStartPre=/home/me/go/bin/dun dund --home /home/me/.dun serve --check --http 192.168.1.76:20003\n",
		"ExecStart=/home/me/go/bin/dun dund --home /home/me/.dun serve --http 192.168.1.76:20003\n",
		"WorkingDirectory=/home/me/.dun\n",
		"Environment=DUN_HOME=/home/me/.dun\n",
		`Environment="X=a b"` + "\n",
		"Restart=on-failure\n",
		"RestartForceExitStatus=75\n", // an upgrade's handover (handoverExit)
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in\n%s", want, u)
		}
	}
	// A path with a space or a % survives systemd's own unquoting.
	if q := quoteUnit("/a b/100%"); q != `"/a b/100%%"` {
		t.Errorf("quote: %s", q)
	}
	if p := servePrefix("/usr/bin/dund"); p != nil {
		t.Errorf("standalone dund prefix: %v", p)
	}
}

// The LaunchAgent: the same command as the systemd unit, as an argument
// array (no shell quoting to get wrong), the PATH the engines need, and
// restart-on-failure. Written as a plist that parses (checked by plutil on a
// Mac; by the shape here).
func TestRenderLaunchAgent(t *testing.T) {
	p := launchd{}.render("dund", "/Users/me/.local/bin/dun", []string{"dund"}, "/Users/me/.dun",
		[]string{"--http", "127.0.0.1:20003"}, []string{"PATH=/opt/homebrew/bin:/usr/bin", "X=a<b"})
	for _, want := range []string{
		"<key>Label</key><string>com.iodesystems.dund</string>",
		"<string>/Users/me/.local/bin/dun</string>\n\t\t<string>dund</string>\n\t\t<string>--home</string>\n\t\t<string>/Users/me/.dun</string>\n\t\t<string>serve</string>\n\t\t<string>--http</string>\n\t\t<string>127.0.0.1:20003</string>",
		"<key>WorkingDirectory</key><string>/Users/me/.dun</string>",
		"<key>DUN_HOME</key><string>/Users/me/.dun</string>",
		"<key>PATH</key><string>/opt/homebrew/bin:/usr/bin</string>",
		"<key>X</key><string>a&lt;b</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>StandardErrorPath</key><string>/Users/me/.dun/dund.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if os.Getenv("PLIST_OUT") != "" {
		_ = os.WriteFile(os.Getenv("PLIST_OUT"), []byte(p), 0o644)
	}
}

// The installer's PATH goes into the service unless one is given.
func TestWithPATH(t *testing.T) {
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/bin")
	if got := withPATH([]string{"X=1"}); len(got) != 2 || got[1] != "PATH=/opt/homebrew/bin:/usr/bin" {
		t.Errorf("withPATH: %v", got)
	}
	if got := withPATH([]string{"PATH=/x"}); len(got) != 1 {
		t.Errorf("a given PATH must be kept: %v", got)
	}
}

// ServicePATH reads back what the systemd unit says (quoted, %-escaped).
func TestServicePATHFromUnit(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the unit format is the Linux one")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := UnitPath(ServiceUnit)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(renderUnit("/bin/dun", nil, "/h", nil, []string{"PATH=/a b/bin:/usr/bin"})), 0o644)
	if got, ok := ServicePATH(ServiceUnit); !ok || got != "/a b/bin:/usr/bin" {
		t.Errorf("ServicePATH = %q %v", got, ok)
	}
}

// A rendered file names its home on the line homeLine gives, so a TUI can
// tell the user's service from one for another DUN_HOME.
func TestHomeLine(t *testing.T) {
	u := renderUnit("/bin/dun", []string{"dund"}, "/home/me/.dun", nil, nil)
	if !strings.Contains(u, systemd{}.homeLine("/home/me/.dun")) || strings.Contains(u, systemd{}.homeLine("/tmp/rc")) {
		t.Errorf("systemd home line in\n%s", u)
	}
	p := launchd{}.render("dund", "/bin/dun", []string{"dund"}, "/Users/me/.dun", nil, nil)
	if !strings.Contains(p, launchd{}.homeLine("/Users/me/.dun")) || strings.Contains(p, launchd{}.homeLine("/tmp/rc")) {
		t.Errorf("launchd home line in\n%s", p)
	}
}
