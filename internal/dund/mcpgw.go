package dund

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// dund's MCP gateway (dun/gateway.go; docs/mcp-and-tools.md, "One
// connection per server"): the control path. An engine asks for a shared
// server's endpoint with the peer op mcp_share; dund counts the client
// against the engine, whose exit releases it. A login's URL goes to the
// sessions waiting on the start as mcp_login, and an upstream's
// notifications to every attached engine as mcp_notify.

// shareTimeout bounds a share: one may wait on a person's login, which the
// broker bounds at five minutes.
var shareTimeout = 6 * time.Minute

type gatewayHolder struct {
	mu sync.Mutex
	gw *dun.Gateway
}

// get is the gateway, made first when create; nil when none was made.
func (h *gatewayHolder) get(create bool) *dun.Gateway {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gw == nil && create {
		h.gw = dun.NewGateway(oauthBroker())
	}
	return h.gw
}

// gateway is the service's, started on first use.
func (s *Service) gateway() *dun.Gateway { return s.gwh.get(true) }

// mcpShare answers mcp_share and mcp_release for ses's engine eng.
func (s *Service) mcpShare(ses *session, eng Engine, op string, req *proto.McpShareReq) (*proto.McpShareRep, error) {
	gw := s.gateway()
	if op == "mcp_release" {
		gw.Release(req.Release)
		return nil, nil
	}
	var sr dun.ShareRequest
	if err := json.Unmarshal([]byte(req.Request), &sr); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), shareTimeout)
	defer cancel()
	g, err := gw.Share(ctx, dun.GatewayClient{Session: ses.id, Engine: eng,
		Login: func(id, url string) { ses.log.Append(proto.NewEvent(&proto.McpLogin{Server: id, URL: url})) },
		Notify: func(key, method string, params map[string]any) {
			_ = ses.sendLoaded(proto.NewCommand("", &proto.McpNotify{Key: key, Method: method, Params: params}))
		}}, sr)
	if err != nil {
		return nil, err
	}
	return &proto.McpShareRep{URL: g.URL, Bearer: g.Bearer, Key: g.Key}, nil
}

// releaseEngine gives back every gateway client of an engine that exited.
func (s *Service) releaseEngine(eng Engine) {
	if gw := s.gwh.get(false); gw != nil {
		gw.ReleaseEngine(eng)
	}
}
