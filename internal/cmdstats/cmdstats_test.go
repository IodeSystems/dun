package cmdstats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	Record("/context", SurfaceTUI)
	Record("RETRY", SurfaceEngine)
	Record("/rm -rf secret", SurfaceTUI) // a name only: not a command name
	Record("", SurfaceTUI)

	f, err := os.Open(filepath.Join(home, "stats", "commands.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]string
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		got = append(got, m)
	}
	if len(got) != 2 || got[0]["cmd"] != "/context" || got[0]["surface"] != "tui" ||
		got[1]["cmd"] != "/retry" || got[1]["surface"] != "engine" {
		t.Fatalf("lines: %v", got)
	}
	if _, err := time.Parse(time.RFC3339, got[0]["ts"]); err != nil {
		t.Errorf("ts: %v", err)
	}
	if len(got[0]) != 3 {
		t.Errorf("extra fields: %v", got[0])
	}
}

// A file that cannot be written does not fail the command.
func TestRecord_Unwritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "stats"), nil, 0o600); err != nil { // a file where the dir goes
		t.Fatal(err)
	}
	Record("/context", SurfaceTUI) // must not panic or block
}
