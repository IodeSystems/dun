package dundapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Live events: Subscription.dund_sessionEvents over graphql-transport-ws on
// /api/graphql (RT-1), one socket per client (RT-3).
//
// A browser cannot put a bearer header on a WebSocket, so the upgrade is not
// bearer-checked; the subscription is gated instead by an HMAC token (RT-2)
// that only an authenticated caller can mint (subscribeToken), binding the
// principal, the session and a timestamp, good for tokenSkew either side.
// The secret lives in memory: a restart voids outstanding tokens, and a
// client re-mints on every (re)subscribe anyway.

const tokenSkew = 5 * time.Minute

type subSigner struct{ key []byte }

func newSigner() *subSigner {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &subSigner{key: k}
}

func (s *subSigner) sign(principal, session string, ts int64) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(principal + "\n" + session + "\n" + strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *subSigner) verify(principal, session string, ts int64, mac string, now time.Time) bool {
	if d := now.Sub(time.Unix(ts, 0)); d > tokenSkew || d < -tokenSkew {
		return false
	}
	return hmac.Equal([]byte(s.sign(principal, session, ts)), []byte(mac))
}

// isUpgrade: the graphql-ws handshake, which carries no bearer.
func isUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/api/graphql" &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

type TokenInput struct {
	SessionPath
}

type TokenOutput struct {
	Body struct {
		Principal string `json:"principal"`
		Timestamp int64  `json:"timestamp"`
		HMAC      string `json:"hmac"`
	}
}

type StreamInput struct {
	Session   string `query:"session" required:"true"`
	After     uint64 `query:"after" doc:"the last seq seen; 0 for the start"`
	Principal string `query:"principal" required:"true" doc:"from subscribeToken"`
	Timestamp int64  `query:"timestamp" required:"true" doc:"from subscribeToken"`
	HMAC      string `query:"hmac" required:"true" doc:"from subscribeToken"`
}

// StreamEvent is one event, the flat JSON frame every transport carries. A
// "snapshot" event replaces what the client shows.
type StreamEvent struct {
	Event map[string]any `json:"event"`
}

func (s *Server) registerStream(api huma.API, g *gat.Gateway) {
	gat.Register(api, g, huma.Operation{OperationID: "subscribeToken", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/subscribe-token",
		Summary: "Mint the token sessionEvents (subscription) needs; good for 5 minutes."},
		func(ctx context.Context, in *TokenInput) (*TokenOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			if _, err := s.svc.Get(p, in.ID); err != nil {
				return nil, fail(err)
			}
			o := &TokenOutput{}
			o.Body.Principal, o.Body.Timestamp = p, time.Now().Unix()
			o.Body.HMAC = s.signer.sign(p, in.ID, o.Body.Timestamp)
			return o, nil
		})
	gat.Subscribe(api, g, huma.Operation{OperationID: "sessionEvents",
		Summary: "A session's events after a seq, then live (snapshot first when too far behind)."},
		func(ctx context.Context, in *StreamInput) (<-chan *StreamEvent, error) {
			if !s.signer.verify(in.Principal, in.Session, in.Timestamp, in.HMAC, time.Now()) {
				return nil, huma.Error401Unauthorized("subscription token invalid or expired: mint a new one")
			}
			evs, err := s.svc.Stream(ctx, in.Principal, in.Session, in.After)
			if err != nil {
				return nil, fail(err)
			}
			out := make(chan *StreamEvent)
			go func() {
				defer close(out)
				for ev := range evs {
					select {
					case out <- &StreamEvent{Event: flat(ev)}:
					case <-ctx.Done():
						return
					}
				}
			}()
			return out, nil
		})
}
