package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/harness"
)

// dun trace names the id's session and role, and prints each line that has
// it: an engine log line and a round-stats line planted under $DUN_HOME.
func TestTraceCmd_FindsPlantedID(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	home := harness.DunHome()
	id := "dun-20261120-150405-7f3a-child#2.fold-14"
	plant := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant(filepath.Join(home, "dund", "engines", "4242.log"),
		"2026/11/20 15:05:00 dun: other line\n2026/11/20 15:05:01 dun: trace "+id+" child#2.fold: finish=stop prompt=9 completion=2 40ms provider_id=gen-1\n")
	plant(filepath.Join(home, "stats", "rounds-2026-11.jsonl"), `{"t":"2026-11-20T15:05:01Z","trace":"`+id+`","prompt":9}`+"\n")

	var out, errw bytes.Buffer
	if code := traceCmd([]string{id}, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errw.String())
	}
	got := out.String()
	for _, want := range []string{"session 20261120-150405", "role child#2.fold", "request 14", "4242.log:2:", "rounds-2026-11.jsonl:1:", "provider_id=gen-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	out.Reset()
	if code := traceCmd([]string{"dun-20261120-150405-7f3a-main-99"}, &out, &errw); code != 1 || !strings.Contains(out.String(), "no line") {
		t.Errorf("an unknown id: exit %d, %s", code, out.String())
	}
}
