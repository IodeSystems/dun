package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/anthropicadmin"
)

func fakeAntBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ant")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// With neither credential it says estimates and how to get more.
func TestSpendLine_Neither(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	got := spendLine(context.Background(), anthropicadmin.Source{KeyFile: spendKeyFile()}, 0, time.Now())
	if !strings.Contains(got, "estimates only") || !strings.Contains(got, "dun spend login") {
		t.Errorf("%q", got)
	}
}

func TestSpendLine_NotLoggedIn(t *testing.T) {
	ant := fakeAntBin(t, `exit 1`)
	got := spendLine(context.Background(), anthropicadmin.Source{Ant: ant}, 0, time.Now())
	if !strings.Contains(got, "not logged in") {
		t.Errorf("%q", got)
	}
}

func TestSpendLogin_TakesTheFlowThatIsThere(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if rc := spendLogin(anthropicadmin.Source{}, false, nil, &out, &errb); rc != 1 || !strings.Contains(errb.String(), "dun doctor --fix") {
		t.Errorf("no ant: %d %q", rc, errb.String())
	}
	out.Reset()
	if rc := spendLogin(anthropicadmin.Source{Ant: fakeAntBin(t, `echo "$@"`)}, true, nil, &out, &errb); rc != 0 ||
		!strings.Contains(out.String(), "auth login --profile admin --scope org:admin --no-browser") {
		t.Errorf("ant: %d %q", rc, out.String())
	}
	if err := os.MkdirAll(filepath.Dir(spendKeyFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spendKeyFile(), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if rc := spendLogin(anthropicadmin.Source{KeyFile: spendKeyFile()}, false, nil, &out, &errb); rc != 0 || !strings.Contains(out.String(), "already set up") {
		t.Errorf("key: %d %q", rc, out.String())
	}
}

func TestSpendBudget(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if msg, err := setSpendBudget("$100"); err != nil || !strings.Contains(msg, "$100.00") {
		t.Fatalf("%q %v", msg, err)
	}
	if loadConfig().SpendBudget != 100 {
		t.Errorf("saved %v", loadConfig().SpendBudget)
	}
	if _, err := setSpendBudget("lots"); err == nil {
		t.Error("lots was taken")
	}
	if _, err := setSpendBudget("off"); err != nil || loadConfig().SpendBudget != 0 {
		t.Errorf("off: %v", err)
	}
}

func TestUsesAnthropic(t *testing.T) {
	if usesAnthropic(dunConfig{URL: "https://llm.example/v1"}) {
		t.Error("not Anthropic")
	}
	if !usesAnthropic(dunConfig{Providers: []providerConfig{{Name: "anthropic", URL: "https://api.anthropic.com/v1"}}}) {
		t.Error("a saved provider counts")
	}
}
