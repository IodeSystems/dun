package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// --help opens with what dun is and how to start, then the flags by group:
// each flag once, one not in a group under advanced.
func TestUsage_StartsWithHowToStartThenGroups(t *testing.T) {
	fs := flag.NewFlagSet("dun", flag.ContinueOnError)
	fs.Bool("setup", false, "the wizard")
	fs.String("model", "", "chat model")
	fs.Bool("pr", false, "removed; does nothing")
	fs.Bool("d", false, "the launcher")
	fs.Bool("brand-new", false, "a flag no group names")
	var b bytes.Buffer
	writeUsage(&b, fs)
	s := b.String()
	if !strings.HasPrefix(s, "dun — a coding assistant") || !strings.Contains(s[:400], "/login") {
		t.Errorf("head:\n%s", s)
	}
	at := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("lacks %q:\n%s", sub, s)
		}
		return i
	}
	if !(at("start and sessions:\n") < at("--setup") && at("--setup") < at("model and provider:\n") &&
		at("--model") < at("advanced:\n") && at("advanced:\n") < at("  -d ") && at("-d ") < at("--brand-new")) {
		t.Errorf("groups:\n%s", s)
	}
	if strings.Contains(s, "workspace and isolation:") {
		t.Errorf("an empty group is listed:\n%s", s)
	}
	for _, f := range []string{"--setup", "--model", "--brand-new"} {
		if n := strings.Count(s, "  "+f+" "); n != 1 {
			t.Errorf("%s listed %d times", f, n)
		}
	}
}

// ship was removed; its flags stay accepted but are not advertised.
func TestUsage_HidesRemovedShipFlags(t *testing.T) {
	fs := flag.NewFlagSet("dun", flag.ContinueOnError)
	fs.Bool("pr", false, "removed; does nothing")
	fs.Bool("ship", true, "removed; does nothing")
	fs.Bool("no-ship", false, "removed; does nothing")
	fs.String("model", "", "chat model")
	var b bytes.Buffer
	writeUsage(&b, fs)
	for _, f := range []string{"--pr", "--ship", "--no-ship", "shipping"} {
		if strings.Contains(b.String(), f) {
			t.Errorf("--help lists removed %q:\n%s", f, b.String())
		}
	}
}
