// Package raglit is dun's client for the raglit daemon's projects API: the
// shared project memory, where a git root is one project and each user has a
// private and a public layer over a maintained public base.
//
// The contract is the daemon's HTTP API (the raglit v0.10.0 daemon: register,
// upload, forget, visibility, owner, savings) and never the daemon's storage.
// A consumer only has to keep speaking the JSON.
//
// Availability is not an error in the sense of an empty answer: a daemon that
// cannot be reached is returned as ErrUnavailable, so a caller can tell "no
// such project" from "could not ask". The daemon is never open: the bearer
// token comes from $RAGLIT_TOKEN (dun's /rag login keeps it there), and a
// daemon without keys answers with none.
package raglit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrUnavailable means the raglit daemon could not be reached — NOT that it
// had nothing to say. Callers must not read it as an empty result.
var ErrUnavailable = errors.New("raglit daemon is not reachable")

// Client talks to a raglit daemon's projects API.
type Client struct {
	BaseURL string
	// HTTP is the transport; the bearer token is added per request.
	HTTP *http.Client
	// Token is the bearer credential, read per request so a rotated token is
	// picked up without rebuilding the client. "" sends no header.
	Token func() string
}

// New returns a client for the daemon at base, defaulting to $RAGLIT_DAEMON
// then the shared per-user daemon address.
func New(base string) *Client {
	if base == "" {
		base = os.Getenv("RAGLIT_DAEMON")
	}
	if base == "" {
		base = "http://127.0.0.1:7420"
	}
	return &Client{
		BaseURL: strings.TrimRight(base, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Token:   func() string { return strings.TrimSpace(os.Getenv("RAGLIT_TOKEN")) },
	}
}

// Registration is the daemon's answer to registering a project: the
// namespace and the three index names the work goes to.
type Registration struct {
	ProjectID string `json:"project_id"`
	Namespace string `json:"namespace"`
	// Base is the maintained public base; Private and Public are the
	// caller's layers over it.
	Base    string `json:"base"`
	Private string `json:"private"`
	Public  string `json:"public"`
	// NewBase/NewPrivate/NewPublic are true where this call created them.
	NewBase    bool `json:"new_base"`
	NewPrivate bool `json:"new_private"`
	NewPublic  bool `json:"new_public"`
}

// Register registers (or re-resolves) a project: its base, created public and
// empty when absent, and the caller's private and public layers over it.
// ProjectID is the project's identity — one per git root commit.
func (c *Client) Register(ctx context.Context, projectID, label string) (Registration, error) {
	body := struct {
		ProjectID string `json:"project_id"`
		Label     string `json:"label,omitempty"`
	}{ProjectID: projectID, Label: label}
	var out Registration
	if err := c.do(ctx, http.MethodPost, "/api/projects/register", nil, &body, &out); err != nil {
		return Registration{}, err
	}
	return out, nil
}

// UploadDoc is a document to upload: Text for plain content, Data base64 for
// binary; Name is the document's identity inside the index.
type UploadDoc struct {
	Name  string `json:"name"`
	Text  string `json:"text,omitempty"`
	Data  string `json:"data,omitempty"`
	Title string `json:"title,omitempty"`
}

// UploadResult is where the document landed and what is working on it.
type UploadResult struct {
	Index string `json:"index"`
	Path  string `json:"path"`
	JobID int64  `json:"job_id"`
}

// Upload sends one document to a layer. Index is the layer name — the
// private, public or base index register handed out. A document's identity
// is its Name, so re-uploading under a name replaces that document.
func (c *Client) Upload(ctx context.Context, index string, doc UploadDoc) (UploadResult, error) {
	q := url.Values{"index": {index}}
	var out UploadResult
	if err := c.do(ctx, http.MethodPost, "/api/upload", q, doc, &out); err != nil {
		return UploadResult{}, err
	}
	return out, nil
}

// Forget drops one document from a layer. raglit has no in-place edit: a
// refine is a forget plus a re-upload under the same name.
func (c *Client) Forget(ctx context.Context, index, path string) error {
	q := url.Values{"index": {index}, "path": {path}}
	var out struct {
		OK bool `json:"ok"`
	}
	return c.do(ctx, http.MethodPost, "/api/forget", q, nil, &out)
}

// Visibility is a layer's visibility: "private" or "public".
type Visibility string

const (
	Private Visibility = "private"
	Public  Visibility = "public"
)

// ACL is the daemon's statement of who owns a layer and its visibility.
type ACL struct {
	Owner      string     `json:"owner"`
	Visibility Visibility `json:"visibility"`
}

// SetVisibility makes a layer private or public. Owner-only: the daemon
// answers 403 for anyone else.
func (c *Client) SetVisibility(ctx context.Context, index string, v Visibility) (ACL, error) {
	body := struct {
		Index      string     `json:"index"`
		Visibility Visibility `json:"visibility"`
	}{Index: index, Visibility: v}
	var out ACL
	if err := c.do(ctx, http.MethodPost, "/api/visibility", nil, body, &out); err != nil {
		return ACL{}, err
	}
	return out, nil
}

// TransferOwner hands a layer to another principal (current owner or admin).
func (c *Client) TransferOwner(ctx context.Context, index, owner string) (ACL, error) {
	body := struct {
		Index string `json:"index"`
		Owner string `json:"owner"`
	}{Index: index, Owner: owner}
	var out ACL
	if err := c.do(ctx, http.MethodPost, "/api/owner", nil, body, &out); err != nil {
		return ACL{}, err
	}
	return out, nil
}

// LayerSavings is one layer's share of what a maintained base would save.
type LayerSavings struct {
	Name       string `json:"name"`
	Documents  int    `json:"documents"`
	Duplicates int    `json:"duplicates"`
}

// Savings is what a maintained base would save a shared project: the copies
// across its layers and the search fan-out it would cut.
type Savings struct {
	ProjectID   string `json:"project_id"`
	Base        string `json:"base"`
	Layers      []LayerSavings `json:"layers"`
	DuplicateDocuments  int     `json:"duplicate_documents"`
	DuplicatePassages   int     `json:"duplicate_passages"`
	DuplicateEmbeddings int     `json:"duplicate_embeddings"`
	DuplicatePageReads  int     `json:"duplicate_page_reads"`
	DuplicateBytes      int64   `json:"duplicate_bytes"`
	ModelSeconds        float64 `json:"model_seconds"`
	// FanoutNow is the search fan-out today; FanoutWithBase, with the base.
	FanoutNow      int    `json:"fanout_now"`
	FanoutWithBase int    `json:"fanout_with_base"`
	// Recommend is the daemon's verdict on folding, with its reason.
	Recommend bool   `json:"recommend"`
	Reason    string `json:"reason"`
	Note      string `json:"note"`
}

// Savings returns the project's savings, found by project ID and/or
// namespace. The daemon answers 404 for an unknown one.
func (c *Client) Savings(ctx context.Context, projectID, namespace string) (Savings, error) {
	q := url.Values{}
	if projectID != "" {
		q.Set("id", projectID)
	}
	if namespace != "" {
		q.Set("ns", namespace)
	}
	var out Savings
	if err := c.do(ctx, http.MethodGet, "/api/projects/savings", q, nil, &out); err != nil {
		return Savings{}, err
	}
	return out, nil
}

// do sends one API request with the bearer token and decodes the answer.
// body, when non-nil, is JSON-encoded; q rides on the URL. A non-OK answer
// carries the daemon's own message, which for these endpoints is the reason
// (no such index, only the owner, ...).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, into any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := c.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Connection refused, DNS, timeout: the daemon is not there. Wrapped so a
		// caller can distinguish it from a daemon that answered with an error.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(detail))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("raglit %s: %s", path, msg)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
