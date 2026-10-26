package main

import "testing"

// The reasoning dialect: DUN_REASONING, else the active provider's when it
// is the endpoint in use, else the config's; an unknown one is refused.
func TestReasoningDialect(t *testing.T) {
	fc := dunConfig{Provider: "corp", Reasoning: "llamacpp",
		Providers: []providerConfig{{Name: "corp", URL: "https://litellm.corp/v1/", Reasoning: "openai"}}}
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
