package main

import (
	"encoding/json"
	"testing"

	"github.com/iodesystems/dun/internal/depver"
)

// The TUI's /version reads `dun tools --json` into depver.Tool's fields:
// the row's JSON names must be the same.
func TestToolRowJSONIsDepverTool(t *testing.T) {
	r := toolRow{Name: "mcpshell", Path: "/b/mcpshell", Version: "v0.11.0", Origin: "release v0.11.0",
		Pin: "v0.12.0", Release: true, Target: "v0.12.0"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got depver.Tool
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != r.dep() {
		t.Errorf("from JSON %+v, want %+v", got, r.dep())
	}
}
