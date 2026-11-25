package tui

import (
	"slices"
	"testing"
)

// The flags reach the engine's argv, and dund's argv filter, for the TUI.
func TestFlags_PassToTheEngine(t *testing.T) {
	o := TuiOpts{Workspace: "/ws", Suggest: true, Rag: "false", Lsp: "false", NoTools: true}
	for _, mode := range []string{"--engine", "--tui"} {
		args := ProcArgs(o, mode)
		for _, want := range []string{"--no-tools", "--rag=false", "--lsp=false"} {
			if !slices.Contains(args, want) {
				t.Errorf("%s argv lacks %s: %v", mode, want, args)
			}
		}
	}
	eng := engineArgs(o)
	if !slices.Contains(eng, "--no-tools") || !slices.Contains(eng, "--rag=false") {
		t.Errorf("dund engine args: %v", eng)
	}
	if slices.Contains(ProcArgs(TuiOpts{Workspace: "/ws", Suggest: true}, "--engine"), "--no-tools") {
		t.Error("--no-tools without being asked")
	}
}

// The key is never on the engine's argv: it is visible in process listings,
// and the engine resolves it from DUN_LLM_KEY env or config.
func TestKeyNotOnArgv(t *testing.T) {
	o := TuiOpts{Workspace: "/ws", Key: "sk-secret", Suggest: true}
	for _, mode := range []string{"--engine", "--tui"} {
		args := ProcArgs(o, mode)
		for i, a := range args {
			if a == "--key" {
				t.Fatalf("%s argv carries --key at %d: %v", mode, i, args)
			}
			if a == "sk-secret" {
				t.Fatalf("%s argv carries the key value at %d: %v", mode, i, args)
			}
		}
	}
}

// Off argv, the key must reach the engine some other way: the dund the TUI
// starts carries it as DUN_LLM_KEY, which the engine dund spawns inherits.
// Without this, a --key user with no DUN_LLM_KEY in the environment would
// have a key that is off argv and never reaches the engine.
func TestKeyOnTheDundEnv(t *testing.T) {
	t.Setenv("DUN_LLM_KEY", "")
	env := dundServeEnv("sk-secret")
	if v := getenv("DUN_LLM_KEY", env); v != "sk-secret" {
		t.Fatalf("dund env DUN_LLM_KEY = %q, want the passed key", v)
	}

	// A key already in the environment is not overridden by the one on the CLI.
	t.Setenv("DUN_LLM_KEY", "env-key")
	env = dundServeEnv("cli-key")
	if v := getenv("DUN_LLM_KEY", env); v != "env-key" {
		t.Fatalf("dund env DUN_LLM_KEY = %q, want the environment's", v)
	}
	// A cleared (empty) environment value is not a key: the passed one fills it.
	t.Setenv("DUN_LLM_KEY", "")
	env = dundServeEnv("cli-key")
	if v := getenv("DUN_LLM_KEY", env); v != "cli-key" {
		t.Fatalf("dund env DUN_LLM_KEY = %q, want the passed key (empty env value is absent)", v)
	}
}
