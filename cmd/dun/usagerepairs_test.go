package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --repairs reads the month's repair lines, in text and in JSON.
func TestUsageRepairs(t *testing.T) {
	noon(t)
	ts := usageNow().Add(-time.Hour).UTC().Format(time.RFC3339)
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	dir := filepath.Join(home, "stats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := `{"kind":"repair","t":"` + ts + `","provider":"p","model":"m1","cause":"empty-reply"}` + "\n" +
		`{"kind":"repair_outcome","t":"` + ts + `","provider":"p","model":"m1","cause":"empty-reply","outcome":"repaired","attempt":1}` + "\n" +
		ledgerLine(time.Hour, "main", "m1", 1000, 0, 100, 0) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rounds-"+usageNow().UTC().Format("2006-01")+".jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if rc := usageCmd([]string{"--repairs"}, &out, &errb); rc != 0 {
		t.Fatal(rc, errb.String())
	}
	for _, want := range []string{"cost of repairs", "repair empty-reply", "p · m1", "round latency by model"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if rc := usageCmd([]string{"--repairs", "--json"}, &out, &errb); rc != 0 {
		t.Fatal(rc, errb.String())
	}
	var rep struct {
		Groups []struct {
			Kind  string
			Count struct{ N, OK int }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(rep.Groups) != 1 || rep.Groups[0].Count.N != 1 || rep.Groups[0].Count.OK != 1 {
		t.Errorf("json %s err %v", out.String(), err)
	}
}
