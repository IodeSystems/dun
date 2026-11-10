package ragd

import (
	"github.com/iodesystems/poly-lsp-mcp/segment"
	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/internal/depver"
)

// codeSeg is raglit.CodeSegmenter over poly-lsp-mcp's segment package, a
// copy of raglit's cmd/raglit/codesegment.go (v0.7.0). It cuts source files
// at their declarations and Markdown at its headings. dund builds it pure Go
// (gotreesitter), without -tags treesitter_cgo (USER 2026-11-09).
//
// It is Models.Code of the code+docs runner only: memory notes and
// strategies are ingested as text (ingestText) and never reach it.
type codeSeg struct{}

var _ raglit.CodeSegmenter = codeSeg{}

// newCodeSeg is the segmenter Open gives the code+docs runner; a test sets
// it to return nil to open a Host as dund was before it had one.
var newCodeSeg = func() raglit.CodeSegmenter { return codeSeg{} }

func (codeSeg) ForPath(path string) (string, bool) { return segment.ForPath(path) }

func (codeSeg) Symbols(lang string, content []byte) ([]raglit.CodeSpan, error) {
	sp, err := segment.Symbols(lang, content)
	return codeSpans(sp), err
}

func (codeSeg) Sections(content []byte) ([]raglit.CodeSpan, error) {
	sp, err := segment.Sections(content)
	return codeSpans(sp), err
}

// Runtime names the walk and the parser under it, as
// "poly-lsp-mcp/segment@v0.5.1 gotreesitter@…". It is in raglit's KindCode
// pool key and in the index's segmenter record (segmenterKey): a new
// poly-lsp-mcp can cut the same file differently on the same parser.
func (codeSeg) Runtime() string {
	return "poly-lsp-mcp/segment@" + segmentVersion() + " " + segment.Runtime()
}

// segmentVersion is poly-lsp-mcp's version in this binary (depver): the
// build info's, "devel" under a directory replace (a go.work), and go.mod's
// pin when the build info lists no modules (a test binary from Go 1.26,
// which read "devel" in CI while a Go 1.27 one read v0.5.0).
func segmentVersion() string { return depver.Version(depver.PolyLSP) }

// codeSpans copies segment.Spans field for field (raglit.CodeSpan is the
// same shape on purpose).
func codeSpans(sp []segment.Span) []raglit.CodeSpan {
	if sp == nil {
		return nil
	}
	out := make([]raglit.CodeSpan, len(sp))
	for i, s := range sp {
		out[i] = raglit.CodeSpan{
			Sym: s.Sym, Class: s.Class,
			StartLine: s.StartLine, EndLine: s.EndLine,
			CommentStartLine: s.CommentStartLine, BodyStartLine: s.BodyStartLine,
		}
	}
	return out
}
