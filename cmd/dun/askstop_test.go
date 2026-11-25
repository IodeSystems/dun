package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// A stop while a question is open suspends it (the next engine asks it
// again); an answer before the stop is taken.
func TestAwaitAnswerOrStop(t *testing.T) {
	answers := make(chan proto.Answer, 1)
	quit := make(chan struct{})
	answers <- proto.Answer{Value: "b", AskID: "q1"}
	if a, err := awaitAnswerOrStop(context.Background(), answers, quit, "q1"); err != nil || a.Value != "b" {
		t.Fatalf("answer %+v %v", a, err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(quit)
	}()
	if _, err := awaitAnswerOrStop(context.Background(), answers, quit, "q1"); !errors.Is(err, harness.ErrSuspended) {
		t.Fatalf("stop: %v, want ErrSuspended", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := awaitAnswerOrStop(ctx, answers, make(chan struct{}), "q1"); errors.Is(err, harness.ErrSuspended) || err == nil {
		t.Fatalf("cancel: %v, want the context's error", err)
	}
}
