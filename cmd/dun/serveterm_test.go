package main

import (
	"strings"
	"testing"
)

func TestReachableURLs(t *testing.T) {
	if got := reachableURLs("127.0.0.1:8734"); len(got) != 1 || got[0] != "http://127.0.0.1:8734" {
		t.Fatalf("specific host: %v", got)
	}
	got := reachableURLs("0.0.0.0:8734")
	if len(got) < 1 || got[0] != "http://127.0.0.1:8734" {
		t.Fatalf("all-interfaces should lead with loopback: %v", got)
	}
	for _, u := range got {
		if !strings.HasSuffix(u, ":8734") {
			t.Fatalf("port not preserved in %q", u)
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8734": true, "localhost:8734": false,
		"0.0.0.0:8734": false, ":8734": false, "192.168.1.76:8734": false,
	}
	for addr, want := range cases {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("loopbackOnly(%q)=%v want %v", addr, got, want)
		}
	}
}
