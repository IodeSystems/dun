package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// An MCP server's documentation, copied into the docs index.
//
// A server may serve resources (resources/list, resources/read): its manual,
// API reference, runbooks. Read live, each one would cost the model a tool
// call it has to know to make. Copied into raglit, they come back the way the
// workspace's own docs do — the proactive search surfaces the one that
// matches the conversation, without a call.
//
// The copy is made when a server is added (/mcp add) and again on
// /mcp refresh <id>, not every session: listing and reading every resource is
// network traffic the user did not ask for. Each text resource becomes one
// file in $DUN_HOME/mcp-docs/<id>/, named by its URI, headed by its title and
// URI so a hit says where it came from, and goes into raglit index
// mcp-<id> in the same store as the docs server's. A resource the server no
// longer lists is forgotten and deleted. Binary resources and skills (see
// skills.go) are skipped.

const (
	mcpDocsMax      = 500     // resources read per refresh
	mcpDocMaxBytes  = 1 << 20 // one resource's text, beyond which it is cut
	groupMCPDocs    = "mcp"
	mcpDocsIndexPre = "mcp-"
)

func mcpDocsRoot() string { return filepath.Join(filepath.Dir(SessionsDir()), "mcp-docs") }

func mcpDocsDir(id string) string { return filepath.Join(mcpDocsRoot(), id) }

// textResource reports whether a resource's type is one worth indexing. No
// type at all is taken as text: servers often omit it for plain docs.
func textResource(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" || strings.HasPrefix(mime, "text/") {
		return true
	}
	for _, t := range []string{"json", "xml", "yaml", "markdown", "javascript", "toml"} {
		if strings.Contains(mime, t) {
			return true
		}
	}
	return false
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// mcpDocName is a resource's file name: readable from the URI, unique by a
// hash of it.
func mcpDocName(uri string) string {
	sum := sha256.Sum256([]byte(uri))
	slug := strings.Trim(unsafeName.ReplaceAllString(uri, "-"), "-.")
	if len(slug) > 60 {
		slug = slug[len(slug)-60:]
	}
	return slug + "-" + hex.EncodeToString(sum[:4]) + ".md"
}

// mcpDocsSink is where synced documents go: write and remove files, ingest
// and forget them in the index.
type mcpDocsSink struct {
	ingest func(index string, paths []string) error
	forget func(index string, paths []string) error
}

// raglitDocsSink runs the raglit CLI against the docs server's store — an
// index role's cli fallback (indexSink) — or returns nil when the docs
// server is not raglit.
func raglitDocsSink(docs Server) *mcpDocsSink {
	if filepath.Base(docs.Command) != "raglit" {
		return nil
	}
	store := raglitStoreFlags(docs.Args)
	run := func(verb, index string, paths []string) error {
		for _, batch := range chunkArgs(paths, ingestBatch) {
			args := append(append([]string{verb}, store...), "--index", index)
			if verb == "ingest" {
				args = append(args, "--now")
			}
			out, err := detach(exec.Command(docs.Command, append(args, batch...)...)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("raglit %s: %v: %s", verb, err, strings.TrimSpace(string(out)))
			}
		}
		return nil
	}
	return &mcpDocsSink{
		ingest: func(index string, paths []string) error { return run("ingest", index, paths) },
		forget: func(index string, paths []string) error { return run("forget", index, paths) },
	}
}

// NoResources is SyncMCPDocs's line for a server with no resources: the
// answer to /mcp refresh, and nothing to say after /mcp add.
const NoResources = "serves no resources — nothing to index"

// SyncMCPDocs copies server id's text resources into the docs index and
// returns a line for the user.
func (h *Harness) SyncMCPDocs(ctx context.Context, id string) (string, error) {
	h.srvMu.Lock()
	s, _ := h.spec(id)
	h.srvMu.Unlock()
	if s.Via == ViaShell {
		return "", fmt.Errorf("%s is scripted (hosted in the shell), and dun indexes the docs only of a server it connects to itself — /mcp mode %s standalone to index them", id, id)
	}
	if !h.mgr.ServerStarted(id) {
		return "", fmt.Errorf("%s is not running — /mcp on %s first", id, id)
	}
	if _, ok := h.mgr.Capabilities(id)["resources"]; !ok {
		return NoResources, nil
	}
	// The mcp.docs extension's index: through its CLI fallback, raglit's
	// need not be running.
	var sink *mcpDocsSink
	for _, e := range h.extensions() {
		if e.Subscribes(HookMCPDocs) {
			if sink = h.indexSink(e); sink != nil {
				break
			}
		}
	}
	if sink == nil {
		return "", fmt.Errorf("the docs index is raglit, and none is configured")
	}
	return syncMCPDocs(ctx, h.mgr, id, mcpDocsDir(id), sink)
}

// resourceReader is the part of mcpmgr.Manager a sync uses.
type resourceReader interface {
	ListResources(ctx context.Context, serverID string) ([]mcpmgr.Resource, error)
	ReadResource(ctx context.Context, serverID, uri string) ([]mcpmgr.ResourceContent, error)
}

func syncMCPDocs(ctx context.Context, rr resourceReader, id, dir string, sink *mcpDocsSink) (string, error) {
	rs, err := rr.ListResources(ctx, id)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	keep := map[string]bool{}
	var changed []string
	skipped, failed, unchanged := 0, 0, 0
	for i, r := range rs {
		if i >= mcpDocsMax {
			skipped += len(rs) - i
			break
		}
		if isSkillFile(r) || !textResource(r.MimeType) {
			skipped++
			continue
		}
		cs, err := rr.ReadResource(ctx, id, r.URI)
		if err != nil {
			failed++
			continue
		}
		var text strings.Builder
		for _, c := range cs {
			if c.Text != "" && textResource(c.MimeType) {
				text.WriteString(c.Text)
				text.WriteString("\n")
			}
		}
		if strings.TrimSpace(text.String()) == "" {
			skipped++
			continue
		}
		body := text.String()
		if len(body) > mcpDocMaxBytes {
			body = body[:mcpDocMaxBytes]
		}
		title := firstNonEmptyStr(r.Title, r.Name, r.URI)
		doc := "# " + title + "\n\nFrom MCP server " + id + ", resource " + r.URI + "\n"
		if r.Description != "" {
			doc += "\n" + r.Description + "\n"
		}
		doc += "\n" + body
		name := mcpDocName(r.URI)
		keep[name] = true
		path := filepath.Join(dir, name)
		if old, err := os.ReadFile(path); err == nil && string(old) == doc {
			unchanged++
			continue
		}
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			return "", err
		}
		changed = append(changed, path)
	}

	var stale []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && !keep[e.Name()] {
			stale = append(stale, filepath.Join(dir, e.Name()))
		}
	}
	index := mcpDocsIndexPre + id
	if len(stale) > 0 {
		if err := sink.forget(index, stale); err != nil {
			return "", err
		}
		for _, p := range stale {
			os.Remove(p)
		}
	}
	if len(changed) > 0 {
		if err := sink.ingest(index, changed); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("docs: %d indexed, %d unchanged, %d removed", len(changed), unchanged, len(stale))
	if skipped > 0 {
		msg += fmt.Sprintf(", %d skipped (binary, empty or skills)", skipped)
	}
	if failed > 0 {
		msg += fmt.Sprintf(", %d unreadable", failed)
	}
	return msg + " · index " + index, nil
}

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// docGroup names a hit's surfacing group: memory notes, skills, imported
// rules, MCP docs, or the workspace's own documents. Each group is ranked on
// its own because raglit scores from different indexes do not compare (see
// groupHits). skill and rules may be nil.
func docGroup(mem *memoryStore, skill func(string) (skillHit, bool), rules func(string) bool) func(string) string {
	root := mcpDocsRoot() + string(os.PathSeparator)
	return func(docID string) string {
		if g := mem.group(docID); g != "" {
			return g
		}
		if skill != nil {
			if _, ok := skill(docID); ok {
				return groupSkill
			}
		}
		if rules != nil && rules(docID) {
			return groupRules
		}
		if strings.HasPrefix(docID, root) {
			return groupMCPDocs
		}
		return ""
	}
}
