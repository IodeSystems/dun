package main

import (
	"context"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// A late answer to a closed question is dropped; this question's answer,
// or one naming no question (an older client), is taken.
func TestAwaitAnswerByAskID(t *testing.T) {
	ch := make(chan proto.Answer, 3)
	ch <- proto.Answer{Value: "stale", AskID: "old"}
	ch <- proto.Answer{Value: "yes", AskID: "q1", By: "unix:1000"}
	a, err := awaitAnswer(context.Background(), ch, "q1")
	if err != nil || a.Value != "yes" || a.By != "unix:1000" {
		t.Fatalf("got %+v %v", a, err)
	}
	ch <- proto.Answer{Value: "any"}
	if a, _ := awaitAnswer(context.Background(), ch, "q2"); a.Value != "any" {
		t.Fatalf("an answer naming no question: %+v", a)
	}
}
