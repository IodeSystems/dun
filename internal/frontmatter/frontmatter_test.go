package frontmatter

import "testing"

func TestSplitFrontmatter_TheShapesSkillFilesUse(t *testing.T) {
	meta, body := Split("---\nname: pdf\ndescription: \"Read \\\"PDFs\\\": forms, text.\"\nlicense: x\nmetadata:\n  nested: y\n---\n# Body\n")
	if meta["name"] != "pdf" || meta["description"] != `Read "PDFs": forms, text.` || body != "# Body\n" {
		t.Errorf("quoted: %q %q", meta, body)
	}
	if _, ok := meta["nested"]; ok {
		t.Error("a nested key is not top-level")
	}
	meta, _ = Split("---\nname: a\ndescription: >\n  folded over\n  two lines\nother: 'it''s'\n---\n")
	if meta["description"] != "folded over two lines" || meta["other"] != "it's" {
		t.Errorf("folded/single-quoted: %q", meta)
	}
	if meta, body := Split("no frontmatter"); len(meta) != 0 || body != "no frontmatter" {
		t.Error("plain text")
	}
}
