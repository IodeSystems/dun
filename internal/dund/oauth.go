package dund

import (
	"context"
	"os"
	"sync"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// dund holds OAuth for every session (dun/oauthbroker.go): one broker, so
// one process refreshes each server's token and one login serves every
// session. A login's URL goes to the asking session's clients as an
// mcp_login event, and to a browser on this machine unless DUN_NO_BROWSER.

var (
	brokerOnce sync.Once
	broker     *dun.OAuthBroker
)

// oauthBroker is the process's broker.
func oauthBroker() *dun.OAuthBroker {
	brokerOnce.Do(func() {
		var open func(string) error
		if os.Getenv("DUN_NO_BROWSER") == "" {
			open = dun.OpenBrowser
		}
		broker = dun.NewOAuthBroker(open)
	})
	return broker
}

// OAuth runs one OAuth request for session ses (nil: no session to show a
// login to).
func (s *Service) OAuth(ctx context.Context, ses *session, req proto.OAuthReq) *proto.OAuthResult {
	spec, err := dun.UnmarshalOAuthSpec(req.Spec)
	if err != nil {
		return &proto.OAuthResult{Error: "oauth: the spec: " + err.Error()}
	}
	var notify func(id, url string)
	if ses != nil {
		notify = func(id, url string) {
			ev := proto.NewEvent(&proto.McpLogin{Server: id, URL: url})
			ses.log.Append(ev)
		}
	}
	res, err := oauthBroker().Do(ctx, dun.OAuthRequest{Op: req.Op, ID: req.ID, URL: req.URL, WWW: req.WWW, Rejected: req.Rejected, Spec: spec}, notify)
	out := &proto.OAuthResult{Authorization: res.Authorization, Revoked: res.Revoked, Status: dun.MarshalOAuthStatus(res.Status)}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}
