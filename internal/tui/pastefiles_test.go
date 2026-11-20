package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Pasted or dropped files become their paths, in every way a file manager
// or a terminal spells them; any other text is left as it is.
func TestPastedFilePaths(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "My File.txt")
	plain := filepath.Join(dir, "notes.md")
	touch(t, spaced)
	touch(t, plain)
	qSpaced := "'" + spaced + "'"
	for _, c := range []struct{ name, in, want string }{
		{"file URI with %20", "file://" + dir + "/My%20File.txt", qSpaced},
		{"file URI, localhost", "file://localhost" + plain, plain},
		{"gnome-copied-files", "copy\nfile://" + dir + "/My%20File.txt\nfile://" + plain, qSpaced + " " + plain},
		{"uri-list, CRLF and comment", "# x\r\nfile://" + plain + "\r\n", plain},
		{"backslash-escaped drag", dir + `/My\ File.txt`, qSpaced},
		{"single-quoted drag", qSpaced + " ", qSpaced},
		{"double-quoted drag", `"` + spaced + `"`, qSpaced},
		{"mix of files on one line", qSpaced + " " + plain + " file://" + plain, qSpaced + " " + plain + " " + plain},
		{"text/plain, one path a line", spaced + "\n" + plain, qSpaced + " " + plain},
	} {
		got, ok := pastedFilePaths(c.in)
		if !ok || joinPaths(got) != c.want {
			t.Errorf("%s: %q -> %q %v, want %q", c.name, c.in, joinPaths(got), ok, c.want)
		}
	}
	for _, in := range []string{
		"hello world",
		"see " + plain,
		plain + " is wrong",
		"/no/such/file.txt",
		"file://otherhost/etc/passwd",
		"'" + plain, // open quote
		"relative/notes.md",
		"",
	} {
		if got, ok := pastedFilePaths(in); ok {
			t.Errorf("%q taken as files: %q", in, got)
		}
	}
	if got := joinPaths([]string{"/a/it's"}); got != `'/a/it'\''s'` {
		t.Errorf("quote in a path: %s", got)
	}
}

// The composer gets the paths; one image file is still its image token;
// text is unchanged.
func TestHandlePaste_Files(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "a b.txt")
	touch(t, doc)
	img := filepath.Join(dir, "shot.png")
	writeImage(t, img, 4, 3)

	paste := func(s string) multilineInput {
		in := newMultilineInput()
		in.width = 200
		in.Focus()
		return in.HandlePaste(s)
	}
	two := paste("copy\nfile://" + dir + "/a%20b.txt\nfile://" + img)
	if v := two.Value(); v != "'"+doc+"' "+img {
		t.Fatalf("two files: %q", v)
	}
	in := paste("copy\nfile://" + img)
	if v := in.Value(); v != "[image 1: shot.png 4×3]" || len(in.Images()) != 1 {
		t.Fatalf("one image file: %q", v)
	}
	txt := paste("just some text")
	if v := txt.Value(); v != "just some text" {
		t.Fatalf("text: %q", v)
	}
}

// ctrl+v prefers the clipboard's file list over image data beside it.
func TestClipboardFiles_PreferPath(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "x.txt")
	touch(t, doc)
	old := clipboardRun
	defer func() { clipboardRun = old }()
	clipboardRun = func(name string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "text/uri-list" && name == "wl-paste" {
				return []byte("file://" + doc + "\n"), nil
			}
			if a == "image/png" {
				return pngBytes(t, 2, 2), nil
			}
		}
		return nil, errors.New("No suitable type of content copied")
	}
	msg, ok := pasteImageCmd()().(clipTextMsg)
	if !ok || msg.err != nil {
		t.Fatalf("ctrl+v: %#v", msg)
	}
	if got, ok := pastedFilePaths(msg.text); !ok || joinPaths(got) != doc {
		t.Fatalf("paths %q from %q", got, msg.text)
	}
}
