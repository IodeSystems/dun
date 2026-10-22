package dun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

func writeSkill(t *testing.T, root, dir, md string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSplitFrontmatter_TheShapesSkillFilesUse(t *testing.T) {
	meta, body := splitFrontmatter("---\nname: pdf\ndescription: \"Read \\\"PDFs\\\": forms, text.\"\nlicense: x\nmetadata:\n  nested: y\n---\n# Body\n")
	if meta["name"] != "pdf" || meta["description"] != `Read "PDFs": forms, text.` || body != "# Body\n" {
		t.Errorf("quoted: %q %q", meta, body)
	}
	if _, ok := meta["nested"]; ok {
		t.Error("a nested key is not top-level")
	}
	meta, _ = splitFrontmatter("---\nname: a\ndescription: >\n  folded over\n  two lines\nother: 'it''s'\n---\n")
	if meta["description"] != "folded over two lines" || meta["other"] != "it's" {
		t.Errorf("folded/single-quoted: %q", meta)
	}
	if meta, body := splitFrontmatter("no frontmatter"); len(meta) != 0 || body != "no frontmatter" {
		t.Error("plain text")
	}
}

// Later roots win by name; a skill without a description is dropped; a
// user-only skill is listed for /skill but not offered to the model.
func TestSkills_PrecedenceAndPromptBlock(t *testing.T) {
	home := t.TempDir()
	defer func(f func() (string, error)) { userHomeDir = f }(userHomeDir)
	userHomeDir = func() (string, error) { return home, nil }
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	proj := t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "---\nname: deploy\ndescription: user deploy\n---\nuser body")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "nodesc", "---\nname: nodesc\n---\nx")
	writeSkill(t, filepath.Join(home, ".claude", "skills", "synced", "bucket"), "deep", "---\nname: deep\ndescription: nested\n---\n")
	writeSkill(t, filepath.Join(home, ".dun", "skills"), "manual", "---\nname: manual\ndescription: by hand\ndisable-model-invocation: true\n---\n")
	writeSkill(t, filepath.Join(proj, ".dun", "skills"), "deploy", "---\nname: deploy\ndescription: project deploy\n---\nproject body, see ref.md")
	os.WriteFile(filepath.Join(proj, ".dun", "skills", "deploy", "ref.md"), []byte("the reference"), 0o644)

	h := newNoteHarness(t)
	h.mgr, h.cfg.ConfigDir = mcpmgr.NewManager(), proj
	var names []string
	for _, s := range h.Skills() {
		names = append(names, s.Name+"="+s.Source)
	}
	if strings.Join(names, ",") != "deploy=project,manual=user" {
		t.Fatalf("skills = %v", names)
	}
	block := skillsSystemBlock(h.Skills())
	if !strings.Contains(block, "- deploy: project deploy") || strings.Contains(block, "manual") {
		t.Errorf("block:\n%s", block)
	}
	if out := h.loadSkill(context.Background(), "deploy", ""); !strings.HasPrefix(out, "project body") || strings.Contains(out, "description:") {
		t.Errorf("body: %q", out)
	}
	if out := h.loadSkill(context.Background(), "deploy", "../../../ref.md"); out != "the reference" {
		t.Errorf("a file stays inside the skill: %q", out)
	}
	if out := h.loadSkill(context.Background(), "nope", ""); !strings.Contains(out, "have: deploy, manual") {
		t.Errorf("unknown: %q", out)
	}
}

// fakeResources serves resources from a map, in key order of uris.
type fakeResources struct {
	uris []string
	text map[string]string
}

func (f *fakeResources) ListResources(context.Context, string) ([]mcpmgr.Resource, error) {
	var out []mcpmgr.Resource
	for _, u := range f.uris {
		out = append(out, mcpmgr.Resource{URI: u, Name: filepathBase(u)})
	}
	return out, nil
}

func (f *fakeResources) ReadResource(_ context.Context, _, uri string) ([]mcpmgr.ResourceContent, error) {
	t, ok := f.text[uri]
	if !ok {
		return nil, fmt.Errorf("no %s", uri)
	}
	return []mcpmgr.ResourceContent{{URI: uri, Text: t}}, nil
}

func filepathBase(u string) string { return u[strings.LastIndex(u, "/")+1:] }

func TestDiscoverMCPSkills_ReadsSkillMDAndListsItsFiles(t *testing.T) {
	f := &fakeResources{uris: []string{"skill://review/SKILL.md", "skill://review/checklist.md", "docs://guide"},
		text: map[string]string{"skill://review/SKILL.md": "---\nname: review\ndescription: review a change\n---\nsteps"}}
	rs, _ := f.ListResources(context.Background(), "corp")
	got := discoverMCPSkills(context.Background(), f, "corp", rs)
	if len(got) != 1 || got[0].Name != "review" || got[0].Source != "mcp:corp" || strings.Join(got[0].Files, ",") != "checklist.md" {
		t.Fatalf("%+v", got)
	}
}

// Text resources become files in the index; skills and binaries do not; a
// resource the server drops is forgotten; an unchanged one is not re-ingested.
func TestSyncMCPDocs_IndexesForgetsAndSkips(t *testing.T) {
	f := &fakeResources{uris: []string{"docs://guide", "docs://api", "skill://x/SKILL.md"},
		text: map[string]string{"docs://guide": "# Guide\nrun it", "docs://api": "GET /v1"}}
	var ingested, forgot []string
	sink := &mcpDocsSink{
		ingest: func(index string, p []string) error { ingested = append(ingested, p...); return checkIndex(index) },
		forget: func(index string, p []string) error { forgot = append(forgot, p...); return checkIndex(index) },
	}
	dir := t.TempDir()
	msg, err := syncMCPDocs(context.Background(), f, "corp", dir, sink)
	if err != nil || len(ingested) != 2 || !strings.Contains(msg, "2 indexed") || !strings.Contains(msg, "1 skipped") {
		t.Fatalf("%q %v %v", msg, err, ingested)
	}
	b, _ := os.ReadFile(ingested[0])
	if !strings.Contains(string(b), "From MCP server corp, resource docs://") {
		t.Errorf("a doc says where it came from:\n%s", b)
	}
	f.uris = []string{"docs://guide"}
	ingested = nil
	msg, err = syncMCPDocs(context.Background(), f, "corp", dir, sink)
	if err != nil || len(ingested) != 0 || len(forgot) != 1 || !strings.Contains(msg, "1 unchanged, 1 removed") {
		t.Fatalf("second sync: %q %v ingested %v forgot %v", msg, err, ingested, forgot)
	}
	if left, _ := os.ReadDir(dir); len(left) != 1 {
		t.Errorf("the dropped doc's file must go: %v", left)
	}
}

func checkIndex(index string) error {
	if index != "mcp-corp" {
		return fmt.Errorf("index %q", index)
	}
	return nil
}

func TestDocGroup_MCPDocsAreTheirOwnGroup(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	g := docGroup(nil)
	if g(filepath.Join(mcpDocsDir("corp"), "a.md")) != groupMCPDocs || g("/ws/README.md") != "" {
		t.Error("grouping")
	}
}
