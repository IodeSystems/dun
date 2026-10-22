package proto

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// PROTO_CAPTURE=<file> decodes a recorded `dun -p` stdout strictly: every
// line an Event, every field known. A check to run against a live engine, not
// part of the default suite.
func TestDecodeCapture(t *testing.T) {
	path := os.Getenv("PROTO_CAPTURE")
	if path == "" {
		t.Skip("PROTO_CAPTURE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	Strict = true
	defer func() { Strict = false }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	types := map[string]int{}
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Errorf("%.200s: %v", sc.Text(), err)
			continue
		}
		if _, known := eventTypes[ev.Type]; !known {
			t.Errorf("unregistered type %q", ev.Type)
		}
		types[ev.Type]++
	}
	t.Logf("decoded: %v", types)
}
