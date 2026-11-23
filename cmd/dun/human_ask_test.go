package main

import (
	"os"
	"strings"
	"testing"
)

// swapStdin points os.Stdin at a pipe and returns a restore function.
func swapStdin(t *testing.T, line string) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		w.Close()
		os.Stdin = old
	})
	if _, err := w.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Capture stderr so the prompt output does not pollute test output.
func swapStderr(t *testing.T) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() {
		w.Close()
		os.Stderr = old
		// Drain.
		buf := make([]byte, 4096)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
	})
}

func TestHumanAsk_NoCustomRefusesFreeText(t *testing.T) {
	swapStderr(t)
	swapStdin(t, "yes, please")
	_, err := humanAsk(nil, "Q", []string{"a", "b"}, false, true, nil)
	if err == nil {
		t.Fatal("free text must be refused under noCustom")
	}
	if !strings.Contains(err.Error(), "number") {
		t.Errorf("error should mention a number: %v", err)
	}
}

func TestHumanAsk_NoCustomAcceptsNumber(t *testing.T) {
	swapStderr(t)
	swapStdin(t, "2")
	ans, err := humanAsk(nil, "Q", []string{"a", "b"}, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "b" {
		t.Errorf("got %q, want %q", ans, "b")
	}
}

func TestHumanAsk_FreeTextAllowedWithoutNoCustom(t *testing.T) {
	swapStderr(t)
	swapStdin(t, "yes, please")
	ans, err := humanAsk(nil, "Q", []string{"a", "b"}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "yes, please" {
		t.Errorf("got %q, want the free text", ans)
	}
}
