package ragd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/harness"
)

// embedserved.go: whether the provider serves the default embed model
// (docs/rag.md "Defaults"). With no facets.embed, embed is
// dunconfig.DefaultEmbedModel only where the provider lists it; elsewhere
// embed is off and search is full-text only. An explicit facets.embed is
// never checked: it is used, and its errors are kept.

// CapEmbedServed is the record fact "true" or "false": the provider lists
// (or, after a runtime refusal, no longer serves) the model. It is kept in
// the capability record of (endpoint, model) and trusted for servedTTL.
const CapEmbedServed = "embed.served"

const servedTTL = 24 * time.Hour

// embedOffSaid is the one line dund logs when the default embed model is off.
func embedOffSaid(provider string) string {
	return fmt.Sprintf("embed off: %s has no %s; search uses full-text only (set facets.embed to use another model)",
		provider, dunconfig.DefaultEmbedModel)
}

// ResolveServed is Resolve with the default embed model dropped where the
// provider does not serve it. The answer comes from the capability record
// at capsDir when fresh, else one GET {url}/models, which is then recorded.
// A listing that cannot be read leaves embed on (unknown is not "no"; the
// runtime guard of embedRoute catches a refusal) and is not recorded.
func ResolveServed(ctx context.Context, cfg dunconfig.DunConfig, capsDir string) Roles {
	r := Resolve(cfg)
	f := cfg.Resolve(dunconfig.FacetEmbed, nil, nil)
	if !f.On() || f.Source != dunconfig.SourceDefault {
		return r
	}
	r.EmbedDefault = true
	if served, known := embedServed(ctx, capsDir, r.Models.Embed); known && !served {
		r.Models.Embed = raglit.ModelRole{}
		for i, s := range r.Said {
			if strings.HasPrefix(s, dunconfig.FacetEmbed+":") {
				r.Said[i] = embedOffSaid(f.Provider)
			}
		}
	}
	return r
}

// embedServed reads the record, else asks the provider. known is false when
// neither gave an answer.
func embedServed(ctx context.Context, capsDir string, role raglit.ModelRole) (served, known bool) {
	snap := llm.NewClient(role.BaseURL, "", role.Model).Capabilities()
	if capsDir != "" {
		if f, ok := harness.LoadCapsIn(capsDir, snap.Endpoint, snap.Model).Facts[CapEmbedServed]; ok && time.Since(f.At) < servedTTL {
			return f.Value == "true", true
		}
	}
	ids, err := listModelIDs(ctx, role.BaseURL, role.APIKey)
	if err != nil {
		return true, false
	}
	for _, id := range ids {
		if id == role.Model {
			served = true
			break
		}
	}
	recordServed(capsDir, role, served, fmt.Sprintf("GET /models listed %d models", len(ids)))
	return served, true
}

func recordServed(capsDir string, role raglit.ModelRole, served bool, evidence string) {
	if capsDir == "" {
		return
	}
	snap := llm.NewClient(role.BaseURL, "", role.Model).Capabilities()
	rec := harness.LoadCapsIn(capsDir, snap.Endpoint, snap.Model)
	rec.Facts[CapEmbedServed] = llm.Fact{Value: fmt.Sprint(served), Source: llm.SourceProbed, Evidence: evidence, At: time.Now()}
	_ = harness.StoreCapsIn(capsDir, rec)
}

// listModelIDs is the model ids of GET {base}/models (the OpenAI shape:
// {"data":[{"id":...}]}).
func listModelIDs(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &body); err != nil || body.Data == nil {
		return nil, fmt.Errorf("models: no listing")
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

// embedGuard turns the default embed model off at runtime when the provider
// refuses it as unknown: the first such refusal records it (recordServed) and
// logs once; later calls fail at once, without a request.
type embedGuard struct {
	role    raglit.ModelRole
	capsDir string
	logf    func(string, ...any)
	off     atomic.Bool
}

func (g *embedGuard) isOff() bool { return g != nil && g.off.Load() }

// refused reports whether err is a provider saying the model does not exist.
func refused(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "status 400") &&
		(strings.Contains(s, "does not exist") || strings.Contains(s, "unknown model") || strings.Contains(s, "model not found"))
}

func (g *embedGuard) see(err error) {
	if err == nil || !refused(err) || g.off.Swap(true) {
		return
	}
	recordServed(g.capsDir, g.role, false, "embed call refused: "+err.Error())
	g.logf("dund: rag: embed off: the provider does not serve %s; search uses full-text only (set facets.embed to use another model)", g.role.Model)
}

var errEmbedOff = fmt.Errorf("rag: embed off: the provider does not serve the default embed model (set facets.embed)")
