package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

func TestSkillRefs(t *testing.T) {
	text := "See references/forms.md and [the guide](guide.md#top), not https://x.io/a.md.\n" +
		"Run `scripts/fill.py`. Also @shared/rules.md, e.g. node.js, mail a@b.md.\n" +
		"```\npython scripts/x.py @not/an/include.md\n```\n" +
		"![img](pic.png) `@code/span.md`\n"
	var got []string
	for _, r := range SkillRefs(text) {
		k := "link"
		if r.Include {
			k = "include"
		}
		if r.Bare {
			k += "?"
		}
		if text[r.Start:r.End] == "" {
			t.Errorf("empty span for %+v", r)
		}
		got = append(got, k+":"+r.Target+"="+r.Text)
	}
	want := "link:references/forms.md=references/forms.md link:guide.md=the guide link:scripts/fill.py=scripts/fill.py " +
		"include:shared/rules.md=@shared/rules.md link?:e.g=e.g link?:node.js=node.js " +
		"link:scripts/x.py=scripts/x.py"
	if strings.Join(got, " ") != want {
		t.Errorf("refs:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
}

func TestSkillLinkPath(t *testing.T) {
	has := map[string]bool{"references/a.md": true, "b.md": true}
	ex := func(p string) bool { return has[p] }
	for _, c := range []struct{ from, t, want string }{
		{"SKILL.md", "references/a.md", "references/a.md"},
		{"references/a.md", "b.md", "b.md"},            // not beside it: the folder's
		{"references/a.md", "a.md", "references/a.md"}, // beside it
		{"references/a.md", "gone.md", "references/gone.md"},
	} {
		if got, ok := SkillLinkPath(c.from, c.t, ex); !ok || got != c.want {
			t.Errorf("%s → %s: %q %v", c.from, c.t, got, ok)
		}
	}
	if _, ok := SkillLinkPath("SKILL.md", "../../etc/passwd", ex); ok {
		t.Error("a path out of the folder is a link")
	}
}

// The engine control's reader: SKILL.md's front matter apart, the folder's
// files, includes to depth 3 with each file once, and no path out of the
// skill's folder, by .. or by a symlink.
func TestSkillFile(t *testing.T) {
	home := t.TempDir()
	defer func(f func() (string, error)) { userHomeDir = f }(userHomeDir)
	userHomeDir = func() (string, error) { return home, nil }
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	root := filepath.Join(home, ".dun", "skills")
	writeSkill(t, root, "forms", "---\nname: forms\ndescription: |\n  fill\n  forms\n---\nRead references/a.md.\n@inc/one.md\n")
	dir := filepath.Join(root, "forms")
	os.MkdirAll(filepath.Join(dir, "references"), 0o755)
	os.MkdirAll(filepath.Join(dir, "inc"), 0o755)
	os.WriteFile(filepath.Join(dir, "references", "a.md"), []byte("the a file"), 0o644)
	os.WriteFile(filepath.Join(dir, "inc", "one.md"), []byte("one @inc/two.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "inc", "two.md"), []byte("two @inc/three.md @inc/one.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "inc", "three.md"), []byte("three @inc/four.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "inc", "four.md"), []byte("four"), 0o644)
	secret := filepath.Join(home, "secret.md")
	os.WriteFile(secret, []byte("secret"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "escape.md"))

	h := newNoteHarness(t)
	h.mgr, h.cfg.ConfigDir = mcpmgr.NewManager(), t.TempDir()
	ctx := context.Background()
	v, err := h.SkillFile(ctx, "forms", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.File != "SKILL.md" || v.Name != "forms" || v.Description != "fill forms" || strings.Contains(v.Text, "description") {
		t.Errorf("SKILL.md: %+v", v)
	}
	if !strings.Contains(strings.Join(v.Files, ","), "references/a.md") {
		t.Errorf("files: %v", v.Files)
	}
	if len(v.Includes) != 3 || v.Includes["inc/three.md"] == "" || v.Includes["inc/four.md"] != "" {
		t.Errorf("includes (depth 3, each once): %v", v.Includes)
	}
	if v, err := h.SkillFile(ctx, "forms", "references/a.md"); err != nil || v.Text != "the a file" {
		t.Errorf("a file: %q %v", v.Text, err)
	}
	for _, bad := range []string{"../../secret.md", "/etc/passwd", "escape.md", "references/../../x"} {
		if v, err := h.SkillFile(ctx, "forms", bad); err == nil || strings.Contains(v.Text, "secret") {
			t.Errorf("%s: read %q", bad, v.Text)
		}
	}
	if _, err := h.SkillFile(ctx, "nope", ""); err == nil {
		t.Error("no such skill: no error")
	}
}
