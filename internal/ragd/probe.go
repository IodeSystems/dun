package ragd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/raglit"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
)

// probe.go: the model-role probes (docs/rag.md, slice 3). Each runs on
// demand, spends a few requests on the role's model, and writes what it
// measured into agentkit's capability record for that (endpoint, model): the
// record /model probe writes (capstore.go). The record is an open keyed map of
// facts, so these are keys of their own, with source probed.

// Keys a role probe writes.
const (
	CapEmbedDimension     = "embed.dimension"     // int: length of one vector
	CapEmbedBatchMax      = "embed.batch_max"     // int: the largest batch accepted, of 1, 8, 32, 64
	CapEmbedLatency       = "embed.latency"       // "<batch>:<ms>ms" per batch tried, space-separated
	CapEmbedDeterministic = "embed.deterministic" // bool: the same input twice gives cosine >= 0.9999
	CapOCRCER             = "ocr.cer"             // float: character error rate on the fixture image
	CapSegmentSchema      = "segment.schema"      // bool: raglit's segmenter got valid fragments
)

// embedBatches are the batch sizes the embed probe tries, in order; it stops
// at the first that fails.
var embedBatches = []int{1, 8, 32, 64}

// sameVector is the cosine at or above which two vectors count as the same.
const sameVector = 0.9999

// ProbeResult is one role's probe: its line from Resolve, and the record as
// stored after it (empty Caps when the role is off).
type ProbeResult struct {
	Facet string
	Said  string
	Caps  llm.Capabilities
	Err   error
}

// Report is the result as text: the role's line, then the record (CapsReport).
func (p ProbeResult) Report() string {
	if p.Caps.Model == "" {
		return p.Said
	}
	s := p.Said + "\n" + dun.CapsReport(p.Caps)
	if p.Err != nil {
		s += "\nprobe stopped: " + p.Err.Error()
	}
	return s
}

// Facets are the roles in the order probes run.
var Facets = []string{dunconfig.FacetEmbed, dunconfig.FacetVision, dunconfig.FacetSegment, dunconfig.FacetIdentity}

// Probe probes facet ("" is every role) as cfg resolves it, and stores each
// record in the capability store at capsDir. A role that is off is reported
// and not probed.
func Probe(ctx context.Context, cfg dunconfig.DunConfig, capsDir, facet string) ([]ProbeResult, error) {
	names := Facets
	if facet != "" {
		if !isFacet(facet) {
			return nil, fmt.Errorf("rag: no role %q (%s)", facet, strings.Join(Facets, ", "))
		}
		names = []string{facet}
	}
	r := Resolve(cfg)
	var out []ProbeResult
	for _, name := range names {
		role := roleOf(r.Models, name)
		res := ProbeResult{Facet: name, Said: saidOf(r.Said, name)}
		if role.Model != "" {
			res.Caps, res.Err = probeRole(ctx, capsDir, name, role)
		}
		out = append(out, res)
	}
	return out, nil
}

func isFacet(s string) bool {
	for _, f := range Facets {
		if f == s {
			return true
		}
	}
	return false
}

func roleOf(m raglit.Models, name string) raglit.ModelRole {
	switch name {
	case dunconfig.FacetEmbed:
		return m.Embed
	case dunconfig.FacetVision:
		return m.Vision
	case dunconfig.FacetSegment:
		return m.Segment
	case dunconfig.FacetIdentity:
		return m.Identity
	}
	return raglit.ModelRole{}
}

func saidOf(said []string, name string) string {
	for _, s := range said {
		if strings.HasPrefix(s, name+":") {
			return s
		}
	}
	return name + ":"
}

// probeClient is a client for one probe: a 5xx is not retried and a 429 is
// waited on for a bounded time, so a probe answers rather than hangs.
func probeClient(role raglit.ModelRole) *llm.Client {
	c := llm.NewClient(role.BaseURL, role.APIKey, role.Model)
	c.Retry5xxAttempts = 1
	c.RetryBudget = 30 * time.Second
	return c
}

// probeRole runs one role's probe and stores what it learned, also when it
// stopped part way.
func probeRole(ctx context.Context, capsDir, name string, role raglit.ModelRole) (llm.Capabilities, error) {
	c := probeClient(role)
	snap := c.Capabilities()
	rec := dun.LoadCapsIn(capsDir, snap.Endpoint, snap.Model)
	if name == dunconfig.FacetIdentity {
		// The chat probes, as /model probe runs them.
		c.SetCapabilities(rec)
		caps, err := llm.ProbeCapabilities(ctx, c, llm.ProbeOpts{})
		if serr := dun.StoreCapsIn(capsDir, caps); serr != nil && err == nil {
			err = serr
		}
		return caps, err
	}
	var facts map[string]llm.Fact
	var err error
	switch name {
	case dunconfig.FacetEmbed:
		facts, err = probeEmbed(ctx, c, role.Model)
	case dunconfig.FacetVision:
		facts, err = probeOCR(ctx, c)
	case dunconfig.FacetSegment:
		facts, err = probeSegment(ctx, c)
	}
	for k, f := range facts {
		if old, ok := rec.Facts[k]; ok && old.Source == llm.SourceHost {
			continue
		}
		rec.Facts[k] = f
	}
	if serr := dun.StoreCapsIn(capsDir, rec); serr != nil && err == nil {
		err = serr
	}
	return rec, err
}

func probed(v, evidence string) llm.Fact {
	return llm.Fact{Value: v, Source: llm.SourceProbed, Evidence: evidence, At: time.Now()}
}

// probeEmbed measures the dimension, the largest batch of embedBatches
// accepted, each batch's latency, and whether the same input twice gives the
// same vector. A failure at batch 1 is an error: nothing about the model was
// learned.
func probeEmbed(ctx context.Context, c *llm.Client, model string) (map[string]llm.Fact, error) {
	facts := map[string]llm.Fact{}
	var lat []string
	var first []float32
	best := 0
	var stop string
	for _, n := range embedBatches {
		in := make([]string, n)
		for i := range in {
			in[i] = fmt.Sprintf("dun embed probe, input %d of %d: the harbor pilot logged a sighting.", i+1, n)
		}
		t0 := time.Now()
		vecs, err := c.Embed(ctx, model, in)
		ms := time.Since(t0).Milliseconds()
		if err == nil {
			for i, v := range vecs {
				if len(v) == 0 || len(v) != len(vecs[0]) {
					err = fmt.Errorf("vector %d has length %d, vector 1 has %d", i+1, len(v), len(vecs[0]))
					break
				}
			}
		}
		if err != nil {
			if n == 1 {
				return facts, fmt.Errorf("embed: one input: %w", err)
			}
			stop = fmt.Sprintf("; %d failed: %s", n, short(err.Error()))
			break
		}
		if first == nil {
			first = vecs[0]
		}
		best = n
		lat = append(lat, fmt.Sprintf("%d:%dms", n, ms))
	}
	facts[CapEmbedDimension] = probed(strconv.Itoa(len(first)), "one input gave a vector of this length")
	facts[CapEmbedBatchMax] = probed(strconv.Itoa(best),
		fmt.Sprintf("tried %s; largest accepted %d%s", joinInts(embedBatches), best, stop))
	facts[CapEmbedLatency] = probed(strings.Join(lat, " "), "wall time per request, by batch size")

	again, err := c.Embed(ctx, model, []string{"dun embed probe, input 1 of 1: the harbor pilot logged a sighting."})
	if err != nil {
		return facts, fmt.Errorf("embed: the same input again: %w", err)
	}
	if len(again) != 1 || len(again[0]) != len(first) {
		facts[CapEmbedDeterministic] = probed("false", "the same input again gave a vector of another length")
		return facts, nil
	}
	cos := cosine(first, again[0])
	facts[CapEmbedDeterministic] = probed(strconv.FormatBool(cos >= sameVector),
		fmt.Sprintf("the same input twice: cosine %.6f (same at >= %.4f)", cos, sameVector))
	return facts, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// ocrFixtureText is what the fixture image says, one line per row.
var ocrFixtureText = []string{"DUN OCR PROBE 2026", "quick brown fox 0123"}

// ocrFixture draws ocrFixtureText in basicfont's 7x13 face, black on white,
// scaled 4x so a vision model reads it at its usual input size. Generated, so
// no binary is kept in the tree.
func ocrFixture() []byte {
	const scale, pad = 4, 8
	face := basicfont.Face7x13
	w := 0
	for _, l := range ocrFixtureText {
		w = max(w, font.MeasureString(face, l).Ceil())
	}
	lh := face.Metrics().Height.Ceil() + 4
	small := image.NewGray(image.Rect(0, 0, w+2*pad, lh*len(ocrFixtureText)+2*pad))
	draw.Draw(small, small.Bounds(), image.White, image.Point{}, draw.Src)
	d := &font.Drawer{Dst: small, Src: image.Black, Face: face}
	for i, l := range ocrFixtureText {
		d.Dot = fixed.P(pad, pad+face.Metrics().Ascent.Ceil()+i*lh)
		d.DrawString(l)
	}
	b := small.Bounds()
	big := image.NewGray(image.Rect(0, 0, b.Dx()*scale, b.Dy()*scale))
	for y := 0; y < big.Bounds().Dy(); y++ {
		for x := 0; x < big.Bounds().Dx(); x++ {
			big.SetGray(x, y, color.Gray{Y: small.GrayAt(x/scale, y/scale).Y})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, big)
	return buf.Bytes()
}

// probeOCR asks the vision model to read the fixture image and scores the
// answer by character error rate: edit distance over the expected length,
// after collapsing whitespace.
func probeOCR(ctx context.Context, c *llm.Client) (map[string]llm.Fact, error) {
	msgs := []llm.Message{{Role: "user", Parts: []llm.ContentPart{
		llm.TextPart("Transcribe all text in this image exactly. Output only the text, nothing else."),
		llm.ImageData("image/png", ocrFixture()),
	}}}
	out, _, err := c.ChatWithOpts(ctx, msgs, nil, &llm.ChatOpts{MaxTokens: 200})
	if err != nil {
		return nil, fmt.Errorf("vision: %w", err)
	}
	want := strings.Join(ocrFixtureText, " ")
	got := strings.Join(strings.Fields(out), " ")
	cer := float64(editDistance(want, got)) / float64(len([]rune(want)))
	return map[string]llm.Fact{CapOCRCER: probed(strconv.FormatFloat(cer, 'f', 3, 64),
		fmt.Sprintf("fixture %q, read %q", want, short(got)))}, nil
}

// editDistance is the Levenshtein distance between a and b, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			sub := prev[j-1]
			if ra[i-1] != rb[j-1] {
				sub++
			}
			cur[j] = min(sub, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// segmentFixture is a short document with two topics.
const segmentFixture = `Harbor operations

The harbor pilot boards each inbound vessel at the outer buoy and guides it to its berth. Pilots work in shifts of twelve hours and log every boarding.

Garden notes

Tomatoes want full sun and deep watering twice a week. Prune the suckers early so the plant puts its energy into fruit.`

// segmentCoverage is the share of the fixture's words the fragments must hold
// for the answer to count as a segmentation of it.
const segmentCoverage = 0.8

// probeSegment runs raglit's own segmenter (its emit_fragments schema and
// fix loop) on the fixture: pass when the model's answer validated without
// the fallback and the fragments hold the document's words.
func probeSegment(ctx context.Context, c *llm.Client) (map[string]llm.Fact, error) {
	res, err := raglit.NewSegmenter(c).SegmentText(ctx, segmentFixture, "")
	if err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	fail := func(why string) map[string]llm.Fact {
		return map[string]llm.Fact{CapSegmentSchema: probed("false", why)}
	}
	if res.Degraded != "" {
		return fail("raglit's segmenter fell back: " + short(res.Degraded)), nil
	}
	var texts []string
	for _, f := range res.Fragments {
		texts = append(texts, f.Text)
	}
	cov := wordCoverage(segmentFixture, strings.Join(texts, " "))
	if cov < segmentCoverage {
		return fail(fmt.Sprintf("%d fragments hold %.0f%% of the document's words (want %.0f%%)",
			len(res.Fragments), 100*cov, 100*segmentCoverage)), nil
	}
	return map[string]llm.Fact{CapSegmentSchema: probed("true",
		fmt.Sprintf("valid emit_fragments: %d fragments, %.0f%% of the document's words", len(res.Fragments), 100*cov))}, nil
}

// wordCoverage is the share of doc's words (as a multiset) found in got.
func wordCoverage(doc, got string) float64 {
	have := map[string]int{}
	for _, w := range strings.Fields(strings.ToLower(got)) {
		have[w]++
	}
	words := strings.Fields(strings.ToLower(doc))
	if len(words) == 0 {
		return 0
	}
	n := 0
	for _, w := range words {
		if have[w] > 0 {
			have[w]--
			n++
		}
	}
	return float64(n) / float64(len(words))
}

// embedDimension is the probed dimension of the embed role's model, from the
// capability store; 0 when it was never probed.
func embedDimension(capsDir string, role raglit.ModelRole) int {
	if role.Model == "" || capsDir == "" {
		return 0
	}
	snap := llm.NewClient(role.BaseURL, "", role.Model).Capabilities()
	n, err := strconv.Atoi(dun.LoadCapsIn(capsDir, snap.Endpoint, snap.Model).Facts[CapEmbedDimension].Value)
	if err != nil {
		return 0
	}
	return n
}

var errNoEmbed = errors.New("rag: no embed model (facets.embed)")
