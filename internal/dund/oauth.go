package dund

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/openurl"
	"github.com/iodesystems/dun/proto"
)

// dund holds OAuth for every session (dun/oauthbroker.go): one broker, so
// one process refreshes each server's token and one login serves every
// session. A login's URL goes to the asking session's clients as an
// mcp_login event, and to a browser on this machine unless DUN_NO_BROWSER.

var (
	brokerOnce sync.Once
	broker     *harness.OAuthBroker
)

// oauthBroker is the process's broker.
func oauthBroker() *harness.OAuthBroker {
	brokerOnce.Do(func() {
		var open func(string) error
		if os.Getenv("DUN_NO_BROWSER") == "" {
			open = openurl.Open
		}
		broker = harness.NewOAuthBroker(open)
	})
	return broker
}

// OAuth runs one OAuth request for session ses (nil: no session to show a
// login to).
func (s *Service) OAuth(ctx context.Context, ses *session, req proto.OAuthReq) *proto.OAuthResult {
	spec, err := harness.UnmarshalOAuthSpec(req.Spec)
	if err != nil {
		return &proto.OAuthResult{Error: "oauth: the spec: " + err.Error()}
	}
	var notify func(id, url string)
	var shown atomic.Bool
	if ses != nil {
		notify = func(id, url string) {
			shown.Store(true)
			ev := proto.NewEvent(&proto.McpLogin{Server: id, URL: url, Phase: proto.LoginStarted})
			ses.log.Append(ev)
		}
	}
	res, err := oauthBroker().Do(ctx, harness.OAuthRequest{Op: req.Op, ID: req.ID, URL: req.URL, WWW: req.WWW, Rejected: req.Rejected, Spec: spec}, notify)
	// This request waited on a login it showed: say how it ended, so a
	// client need not infer it from a later tool listing.
	if shown.Load() {
		ses.log.Append(proto.NewEvent(proto.LoginEnd(req.ID, err)))
	}
	out := &proto.OAuthResult{Authorization: res.Authorization, Revoked: res.Revoked, Status: harness.MarshalOAuthStatus(res.Status)}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}
