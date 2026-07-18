package main

import (
	"os"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := dunConfig{URL: "http://x:9", Model: "my-model", Key: "sk-abcd1234"}
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(); got != c {
		t.Fatalf("round-trip mismatch: %+v != %+v", got, c)
	}
	// The key is secret → 0600.
	st, err := os.Stat(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %v, want 0600", perm)
	}
}

func TestLoadConfig_missingIsZero(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if got := (loadConfig()); got != (dunConfig{}) {
		t.Fatalf("missing config should be zero, got %+v", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "c"); got != "c" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]string{"": "(none)", "abcd": "****", "sk-abcd1234": "****1234"}
	for in, want := range cases {
		if got := maskKey(in); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", in, got, want)
		}
	}
}
