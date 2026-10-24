package main

import (
	"errors"
	"os"

	"golang.org/x/term"
)

// Where /login cannot be typed — a task run headless, `dun --setup` with no
// terminal — the way in is the command line: --setup in a terminal, or the
// provider in the environment for one run (flag > env > config, main.go).

// envProvider is the provider for one run, in the environment.
const envProvider = "  DUN_URL=<OpenAI-compatible base URL> DUN_MODEL=<model> DUN_LLM_KEY=<key, if it needs one>\n" +
	"  (or --url, --model, --key)\n"

// headlessMissing is what a headless task says when there is no provider or
// no model (providerMissing), before anything starts.
func headlessMissing(err error) string {
	if errors.Is(err, errNoModel) {
		return "dun: no model chosen, so the task cannot run. Pick one:\n" +
			"  dun --setup                 in a terminal (saved for next time), or\n" +
			"  DUN_MODEL=<model> dun \"…\"   for this run (or --model)\n"
	}
	return "dun: not logged in to an AI service, so the task cannot run. Log in:\n" +
		"  dun --setup   in a terminal: an address, a key if it needs one, a model (saved for next time)\n" +
		"or for this run, in the environment:\n" + envProvider
}

// setupNoTTY is what `dun --setup` says with no terminal to draw its form on.
const setupNoTTY = "dun --setup needs a terminal, and there is none here. Run it in a terminal,\n" +
	"or give dun the provider in the environment instead:\n" + envProvider

// haveTTY reports whether a form can be drawn: stdin is a terminal, or the
// controlling terminal opens (as Bubble Tea opens it when stdin is a pipe).
func haveTTY() bool {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	f, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	f.Close()
	return true
}
