package dundcli

import (
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
