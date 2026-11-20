package ragd

import (
	"context"
	"fmt"
	"net/http"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/raglit"
)

// searchKey marks a context as an interactive search's (forSearch).
type searchKey struct{}

// forSearch marks ctx as a search's: its query embedding takes the query
// client, not the ingest admission.
func forSearch(ctx context.Context) context.Context {
	return context.WithValue(ctx, searchKey{}, true)
}

// embedRoute is the embed client of every index. A search's query embedding
// (ctx from forSearch) goes on its own client, straight to the model: it never
// waits for an admission slot behind bulk ingest. Every other embed call (a
// job's fragments, a backfill, a reembed) goes through the embed model's
// admission channel in the shared Channels, so ingest across both runners is
// limited per model.
//
// One Embedder per Store (SetEmbedder), so the split is made in the client,
// by the call's context: raglit passes the search's ctx through
// HybridSearchFiltered → VecSearchFiltered → EmbedQuery → Embed.
type embedRoute struct {
	ingest raglit.VectorClient // gated on Channels
	query  raglit.VectorClient // not gated
	guard  *embedGuard         // nil: an explicit embed facet, errors kept
}

func (r embedRoute) Embed(ctx context.Context, model string, input []string) ([][]float32, error) {
	if r.guard.isOff() {
		return nil, errEmbedOff
	}
	var v [][]float32
	var err error
	if ctx.Value(searchKey{}) != nil {
		v, err = r.query.Embed(ctx, model, input)
	} else {
		v, err = r.ingest.Embed(ctx, model, input)
	}
	if r.guard != nil {
		r.guard.see(err)
	}
	return v, err
}

// newEmbedder is the embedder of every index: ingest through raglit's
// Models.Embedder (the ingest retry policy, the admission channel and its 429
// hook), queries on a plain client. m.Embed.Model must be set and m.Channels
// not nil.
func newEmbedder(m raglit.Models, g *embedGuard) *raglit.Embedder {
	q := llm.NewClient(m.Embed.BaseURL, m.Embed.APIKey, m.Embed.Model)
	// The query side's requests are named; ingest's are raglit's clients,
	// which take no header yet (raglit-trace request, 2026-11-20).
	q.Header = http.Header{"X-Trace-Id": {fmt.Sprintf("dund-search-embed-%d", probeSeq.Add(1))}}
	return raglit.NewEmbedder(embedRoute{ingest: m.Embedder().Client, query: q, guard: g}, m.Embed.Model)
}
