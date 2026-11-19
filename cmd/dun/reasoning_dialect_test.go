package main

import (
	"encoding/json"
	"testing"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// The reasoning dialect: DUN_REASONING, else the active provider's when it
// is the endpoint in use, else the config's; an unknown one is refused.
func TestReasoningDialect(t *testing.T) {
	fc := dunconfig.DunConfig{Provider: "corp", Reasoning: "llamacpp",
		Providers: []dunconfig.ProviderConfig{{Name: "corp", URL: "https://litellm.corp/v1/", Reasoning: "openai"}}}
	if d, err := reasoningDialect(fc, "https://litellm.corp/v1"); err != nil || d != "openai" {
		t.Errorf("provider's: %q %v", d, err)
	}
	if d, _ := reasoningDialect(fc, "https://other/v1"); d != "llamacpp" {
		t.Errorf("another endpoint takes the config's: %q", d)
	}
	t.Setenv("DUN_REASONING", "openrouter")
	if d, _ := reasoningDialect(fc, "https://litellm.corp/v1"); d != "openrouter" {
		t.Errorf("env: %q", d)
	}
	t.Setenv("DUN_REASONING", "magic")
	if _, err := reasoningDialect(fc, "https://litellm.corp/v1"); err == nil {
		t.Error("an unknown dialect was taken")
	}
}

// fold_user_into_tool_result is the provider's at the endpoint, read from
// the saved config; off elsewhere.
func TestFoldUserIntoToolResult(t *testing.T) {
	var fc dunconfig.DunConfig
	if err := json.Unmarshal([]byte(`{"provider":"local","providers":[
		{"name":"local","url":"http://127.0.0.1:2/v1","fold_user_into_tool_result":true},
		{"name":"api","url":"https://api.example/v1"}]}`), &fc); err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]bool{
		"http://127.0.0.1:2/v1/":   true,
		"https://api.example/v1":   false,
		"https://other.example/v1": false,
	} {
		if got := foldUserIntoToolResult(fc, u); got != want {
			t.Errorf("%s: %v, want %v", u, got, want)
		}
	}
}
