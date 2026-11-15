package main

import (
	dun "github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// requestEvent is the engine's figures as the protocol's event, with the
// session's so far from f (proto.RequestFold).
func requestEvent(f *proto.RequestFold, r dun.RequestStat) *proto.Request {
	ev := &proto.Request{Phase: r.Phase, TTFTMS: r.TTFTMS, Prompt: r.Prompt, Cached: r.Cached,
		CacheReported: r.CacheReported, PPS: r.PPS, PPSServer: r.PPSServer, Gen: r.Gen, TGS: r.TGS,
		TGSServer: r.TGSServer, Exact: r.Exact, Ctx: r.Ctx, Window: r.Window}
	f.Note(ev)
	return ev
}
