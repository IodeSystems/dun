package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mediaFake sets the hooks: tools maps name to its -version output.
func mediaFake(t *testing.T, goos string, tools map[string]string) {
	t.Helper()
	og, ol, or := mediaGOOS, mediaLookPath, mediaRun
	t.Cleanup(func() { mediaGOOS, mediaLookPath, mediaRun = og, ol, or })
	mediaGOOS = goos
	mediaLookPath = func(n string) (string, error) {
		if _, ok := tools[n]; ok {
			return "/fake/" + n, nil
		}
		return "", errors.New("not found")
	}
	mediaRun = func(p string, _ ...string) (string, error) { return tools[filepath.Base(p)], nil }
}

func osRelease(t *testing.T, body string) {
	t.Helper()
	o := mediaOSRel
	t.Cleanup(func() { mediaOSRel = o })
	mediaOSRel = filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(mediaOSRel, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOSFamily(t *testing.T) {
	mediaFake(t, "linux", nil)
	for body, want := range map[string]string{
		"NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n": "debian",
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n":    "debian",
		"ID=fedora\n": "fedora",
		"ID=rocky\nID_LIKE=\"rhel centos fedora\"\n": "fedora",
		"ID=endeavouros\nID_LIKE=arch\n":             "arch",
		"ID=nixos\n":                                 "",
	} {
		osRelease(t, body)
		if got := osFamily(); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
	mediaOSRel = filepath.Join(t.TempDir(), "missing")
	if osFamily() != "" {
		t.Error("no os-release is unknown")
	}
	mediaGOOS = "darwin"
	if osFamily() != "darwin" {
		t.Error("darwin")
	}
}

func TestMediaInstall(t *testing.T) {
	mediaFake(t, "darwin", map[string]string{"brew": ""})
	if c, fix := mediaInstall(); c != "brew install imagemagick ffmpeg" || !fix {
		t.Errorf("brew: %q %v", c, fix)
	}
	if fixable(finding{"warn", "media", "x", "brew install imagemagick ffmpeg"}) == nil {
		t.Error("brew install is fixable")
	}
	mediaFake(t, "darwin", nil)
	if c, fix := mediaInstall(); fix || !strings.Contains(c, "brew.sh") {
		t.Errorf("no brew: %q %v", c, fix)
	}
	mediaFake(t, "linux", nil)
	osRelease(t, "ID=debian\n")
	if c, fix := mediaInstall(); fix || c != "sudo apt install imagemagick ffmpeg" {
		t.Errorf("debian: %q %v", c, fix)
	}
	osRelease(t, "ID=fedora\n")
	if c, fix := mediaInstall(); fix || !strings.Contains(c, "RPM Fusion") || !strings.HasPrefix(c, "sudo dnf install ImageMagick ffmpeg") {
		t.Errorf("fedora: %q %v", c, fix)
	}
	if fixable(finding{"warn", "media", "x", "sudo dnf install ImageMagick ffmpeg"}) != nil {
		t.Error("sudo must not be run by --fix")
	}
	osRelease(t, "ID=arch\n")
	if c, _ := mediaInstall(); c != "sudo pacman -S imagemagick ffmpeg" {
		t.Error(c)
	}
	mediaFake(t, "windows", nil)
	if c, fix := mediaInstall(); fix || c != "winget install ImageMagick.ImageMagick Gyan.FFmpeg" {
		t.Errorf("windows: %q %v", c, fix)
	}
}

func mediaFindings(t *testing.T) []finding {
	t.Helper()
	osRelease(t, "ID=debian\n")
	d := &doctor{}
	d.media()
	return d.found
}

func hasFinding(fs []finding, level, sub string) bool {
	for _, f := range fs {
		if f.level == level && strings.Contains(f.detail, sub) {
			return true
		}
	}
	return false
}

// magick wins over everything; convert, ffmpeg, vips follow in order.
func TestMediaOrder(t *testing.T) {
	im := "Version: ImageMagick 7.1.1\nCopyright"
	all := map[string]string{"magick": im, "convert": im, "ffmpeg": "ffmpeg version 6\nmore", "vipsthumbnail": "vips-8"}
	mediaFake(t, "linux", all)
	fs := mediaFindings(t)
	if !hasFinding(fs, "ok", "images scale with magick") || !hasFinding(fs, "ok", "audio and video scale with ffmpeg") || !hasFinding(fs, "ok", "ffmpeg /fake/ffmpeg (ffmpeg version 6)") {
		t.Errorf("%+v", fs)
	}
	delete(all, "magick")
	if !hasFinding(mediaFindings(t), "ok", "images scale with convert") {
		t.Error("convert second")
	}
	delete(all, "convert")
	if !hasFinding(mediaFindings(t), "ok", "images scale with ffmpeg") {
		t.Error("ffmpeg third")
	}
	delete(all, "ffmpeg")
	fs = mediaFindings(t)
	if !hasFinding(fs, "ok", "images scale with vipsthumbnail") || !hasFinding(fs, "warn", "no ffmpeg") {
		t.Errorf("vips: %+v", fs)
	}
	// Windows skips convert.
	mediaFake(t, "windows", map[string]string{"convert": im})
	if hasFinding(mediaFindings(t), "ok", "convert") {
		t.Error("windows skips convert")
	}
}

func TestMediaNone(t *testing.T) {
	mediaFake(t, "linux", nil)
	fs := mediaFindings(t)
	if !hasFinding(fs, "warn", "can't be scaled") || !hasFinding(fs, "warn", "no ffmpeg") {
		t.Errorf("%+v", fs)
	}
	for _, f := range fs {
		if f.level == "fail" {
			t.Error("never a fail")
		}
	}
}

// convert that is not ImageMagick is warned about and not chosen.
func TestMediaConvertNotImageMagick(t *testing.T) {
	mediaFake(t, "linux", map[string]string{"convert": "convert: GNU something 1.0", "vipsthumbnail": "vips-8"})
	fs := mediaFindings(t)
	if !hasFinding(fs, "warn", "not ImageMagick") || !hasFinding(fs, "ok", "images scale with vipsthumbnail") || hasFinding(fs, "ok", "convert /fake") {
		t.Errorf("%+v", fs)
	}
}
