package main

import (
	"os"
	"testing"
)

func TestProbeKittyNotTTY(t *testing.T) {
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Skip(err.Error())
	}
	defer f.Close()
	if probeKitty(f) {
		t.Fatal("/dev/null is not a tty; probe must report unsupported")
	}
}
