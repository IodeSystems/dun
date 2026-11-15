package dund

import (
	"testing"

	"github.com/iodesystems/dun/proto"
)

// dund says who answered, whatever the client claimed.
func TestStampAnswer(t *testing.T) {
	c := stampAnswer(proto.NewCommand("", &proto.Answer{Value: "a", AskID: "q", By: "someone else"}), "unix:1000")
	if a := c.Payload.(*proto.Answer); a.By != "unix:1000" || a.AskID != "q" {
		t.Fatalf("%+v", a)
	}
}
