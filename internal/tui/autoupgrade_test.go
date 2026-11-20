package tui

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dund"
)

// A TUI older than dund never downgrades it; builds that cannot be ordered
// are said, not acted on; a newer TUI asks once and says so.
func TestCheckDundBuildDirection(t *testing.T) {
	t.Cleanup(func() { attachNow.Store(nil) })
	oldV, oldProbe := buildinfo.Version, probeSelf
	defer func() { buildinfo.Version, probeSelf = oldV, oldProbe }()
	probeSelf = func(string) string { return buildinfo.Version }

	for _, c := range []struct {
		tui, dund string
		asked     int
		say       string
	}{
		{"v0.9.0-2-gaaaaaaa", "v0.9.0-5-gbbbbbbb", 0, "newer than this TUI"},
		{"v0.9.0", "v0.9.1", 0, "newer than this TUI"},
		{"v0.9.0-5-gaaaaaaa", "v0.9.0-5-gbbbbbbb", 0, "cannot tell which is newer"},
		{"v0.9.0-5-gbbbbbbb", "v0.9.0-2-gaaaaaaa", 1, "upgrading it into this build"},
	} {
		buildinfo.Version = c.tui
		upgradeAsked = map[string]bool{}
		svc, err := dund.New(t.TempDir(), dund.ExecSpawner{Bin: "true"})
		if err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(t.TempDir(), "d.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := dund.NewServer(svc, func(net.Conn) (string, error) { return "unix:1", nil })
		asked := 0
		srv.SetBuild(c.dund, "/old/dun", func(string, string) error { asked++; return nil })
		go srv.Serve(ln)
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := checkDundBuild(&attachConn{c: conn}, bufio.NewScanner(conn))
		conn.Close()
		ln.Close()
		svc.Close()
		if asked != c.asked || len(n) != 1 || !strings.Contains(n[0], c.say) {
			t.Errorf("tui %s, dund %s: asked=%d %v, want asked=%d and %q", c.tui, c.dund, asked, n, c.asked, c.say)
		}
	}
}

func TestVersionSlash_ShowsVersions(t *testing.T) {
	oldV := buildinfo.Version
	buildinfo.Version = "v0.9.0"
	defer func() { buildinfo.Version = oldV }()
	dundSeen.Store("v0.8.1")
	defer dundSeen.Store("")
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.engineVersion = "v0.9.0"
	versionSlash(&m, nil)
	var all []string
	for _, c := range m.convo {
		all = append(all, ansi.Strip(c.view()))
	}
	got := strings.Join(all, "\n")
	if !strings.Contains(got, "dund v0.8.1") || !strings.Contains(got, "engine v0.9.0") || !strings.Contains(got, "⚠ this TUI is v0.9.0 but dund is not") {
		t.Errorf("/version:\n%s", got)
	}
}
