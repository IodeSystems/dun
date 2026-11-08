package dun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
)

// RagLink is the way to dund's retrieval (docs/rag.md, slice 5): its
// indexes hold the memory notes (memory.go) and answer the proactive search
// for them (memrecall.go). The session dials dund's socket for each call,
// so a session under dund and a standalone `dun -p` with dund serving reach
// it the same way. An error that wraps ErrRagDown means dund is not there,
// or runs no retrieval: the caller degrades (memrecall.go), it does not
// fail.
type RagLink interface {
	Search(ctx context.Context, q proto.RagSearch) (*proto.RagHits, error)
	Ingest(ctx context.Context, q proto.RagIngest) (*proto.RagIngested, error)
	Forget(ctx context.Context, q proto.RagForget) (*proto.RagForgotten, error)
	Sync(ctx context.Context, q proto.RagSync) (*proto.RagSynced, error)
}

// ErrRagDown is a RagLink's error when dund cannot be reached, or answers
// that it runs no retrieval.
var ErrRagDown = errors.New("dund is not reachable")

// DundRag is the RagLink over dund's socket at sock.
func DundRag(sock string) RagLink { return dundRag{sock: sock} }

type dundRag struct{ sock string }

// ragDialTimeout bounds the connect: a socket with nobody behind it fails at
// once; this is for one that is slow to accept.
const ragDialTimeout = time.Second

// ragReplyTimeout bounds a call with no deadline of its own. A search may
// wait on the embed model; an ingest only queues.
const ragReplyTimeout = 30 * time.Second

func (d dundRag) call(ctx context.Context, payload, out any) error {
	var dl net.Dialer
	dctx, cancel := context.WithTimeout(ctx, ragDialTimeout)
	c, err := dl.DialContext(dctx, "unix", d.sock)
	cancel()
	if err != nil {
		return fmt.Errorf("%w (%s): %v", ErrRagDown, d.sock, err)
	}
	defer c.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(ragReplyTimeout)
	}
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(c).Encode(proto.NewCommand("rag", payload)); err != nil {
		return fmt.Errorf("%w: %v", ErrRagDown, err)
	}
	dec := json.NewDecoder(c)
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %v", ErrRagDown, err)
		}
		if ev.Re != "rag" {
			continue
		}
		if a, ok := ev.Payload.(*proto.Ack); ok {
			if a.Error == "" {
				continue
			}
			// dund.handleRag's refusal when it hosts no indexes.
			if strings.Contains(a.Error, "retrieval is not running") {
				return fmt.Errorf("%w: %s", ErrRagDown, a.Error)
			}
			return errors.New(a.Error)
		}
		b, err := json.Marshal(ev.Payload)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}
}

func (d dundRag) Search(ctx context.Context, q proto.RagSearch) (*proto.RagHits, error) {
	var out proto.RagHits
	return &out, d.call(ctx, &q, &out)
}

func (d dundRag) Ingest(ctx context.Context, q proto.RagIngest) (*proto.RagIngested, error) {
	var out proto.RagIngested
	return &out, d.call(ctx, &q, &out)
}

func (d dundRag) Forget(ctx context.Context, q proto.RagForget) (*proto.RagForgotten, error) {
	var out proto.RagForgotten
	return &out, d.call(ctx, &q, &out)
}

func (d dundRag) Sync(ctx context.Context, q proto.RagSync) (*proto.RagSynced, error) {
	var out proto.RagSynced
	return &out, d.call(ctx, &q, &out)
}
