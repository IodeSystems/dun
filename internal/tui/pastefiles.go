package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// pastefiles.go — pasted or dropped files become their paths (USER
// 2026-11-20: "pasting files should paste their paths").
//
// A file manager's copy arrives as file:// URIs (text/uri-list, or
// x-special/gnome-copied-files with a "copy"/"cut" first line); a drag onto
// a terminal arrives as absolute paths, shell-quoted or backslash-escaped.
// Either is turned into plain paths, space-separated, each quoted when it
// holds a space or a quote. One image file is still an image token
// (imageinput.go): that is the paste a dropped screenshot makes.

// pastedFilePaths is the files a paste names, when every word of it names
// one: a file:// URI (local host), or an absolute or ~ path that exists.
// Anything else is text, and false.
func pastedFilePaths(text string) ([]string, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if s == "" || len(s) > 64<<10 {
		return nil, false
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && (lines[0] == "copy" || lines[0] == "cut") {
		lines = lines[1:]
	}
	var paths []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") { // uri-list comment
			continue
		}
		// A line that is one path as it stands: a file manager's text/plain
		// copy puts one path a line, spaces unescaped.
		if p, ok := existingPath(ln); ok {
			paths = append(paths, p)
			continue
		}
		words, ok := shellWords(ln)
		if !ok {
			return nil, false
		}
		for _, w := range words {
			p, ok := localPath(w)
			if !ok {
				return nil, false
			}
			paths = append(paths, p)
		}
	}
	return paths, len(paths) > 0
}

// localPath is the file a word names: a decoded file:// URI, or an
// absolute or ~ path that exists.
func localPath(w string) (string, bool) {
	if strings.HasPrefix(w, "file://") {
		u, err := url.Parse(w)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			return "", false
		}
		return u.Path, true
	}
	return existingPath(w)
}

// existingPath is p, ~ expanded and cleaned, when it is absolute and exists.
func existingPath(p string) (string, bool) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return filepath.Clean(p), true
}

// shellWords splits a line the way a shell does for quoting: whitespace
// separates, '…' is literal, "…" and a bare backslash escape. An open quote
// is false.
func shellWords(s string) ([]string, bool) {
	var words []string
	var b strings.Builder
	in := false // a word is open (an empty '' is still a word)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if in {
				words = append(words, b.String())
				b.Reset()
				in = false
			}
		case c == '\\':
			in = true
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case c == '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			in = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
		default:
			in = true
			b.WriteByte(c)
		}
	}
	if in {
		words = append(words, b.String())
	}
	return words, true
}

// joinPaths is paths as composer text: space-separated, a path with a
// space, a quote or a backslash in single quotes.
func joinPaths(paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if strings.ContainsAny(p, " \t'\"\\") {
			p = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
		}
		out[i] = p
	}
	return strings.Join(out, " ")
}

// clipboardFileCmds read the clipboard's file list, for ctrl+v: a file
// manager's copy (Dolphin offers image data beside it) is its paths, not
// the image.
var clipboardFileCmds = [][]string{
	{"wl-paste", "--no-newline", "--type", "text/uri-list"},
	{"xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o"},
}

// clipboardFiles is the clipboard's uri-list, when it names files.
func clipboardFiles() (string, bool) {
	for _, c := range clipboardFileCmds {
		out, err := clipboardRun(c[0], c[1:]...)
		if err != nil || len(out) == 0 {
			continue
		}
		if _, ok := pastedFilePaths(string(out)); ok {
			return string(out), true
		}
	}
	return "", false
}
