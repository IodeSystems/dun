package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Media tools. agentkit scales what read_file returns by running external
// tools, looked up with exec.LookPath in this order: magick (ImageMagick 7),
// else convert (ImageMagick 6; skipped on Windows), then ffmpeg (images, and
// the only one for audio and video), then vipsthumbnail (images only); with
// none, a stdlib path scales png, jpeg and gif only. The check mirrors that
// order so doctor names the tool agentkit will use.

// Test hooks.
var (
	mediaGOOS     = runtime.GOOS
	mediaOSRel    = "/etc/os-release"
	mediaLookPath = exec.LookPath
	// mediaRun runs a tool with a short timeout and returns its stdout.
	mediaRun = func(path string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, args...).Output()
		return string(out), err
	}
)

const mediaPkgs = "imagemagick ffmpeg"

type mediaTool struct {
	name    string
	path    string
	version string
	usable  bool // found, and (for convert) really ImageMagick
}

// mediaProbe looks one tool up and reads its version. convert must say
// ImageMagick: on some systems it is another program.
func mediaProbe(name string, args ...string) mediaTool {
	t := mediaTool{name: name}
	p, err := mediaLookPath(name)
	if err != nil {
		return t
	}
	t.path = p
	out, err := mediaRun(p, args...)
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	t.version = strings.TrimSpace(first)
	if err != nil && t.version == "" {
		t.version = "version unreadable: " + err.Error()
	}
	t.usable = true
	if name == "convert" && !strings.Contains(out, "ImageMagick") {
		t.usable = false
	}
	return t
}

// osFamily is "darwin", "windows", or the Linux distro family from
// os-release ID and ID_LIKE: debian, fedora, arch; "" when unknown.
func osFamily() string {
	switch mediaGOOS {
	case "darwin", "windows":
		return mediaGOOS
	case "linux":
	default:
		return ""
	}
	b, err := os.ReadFile(mediaOSRel)
	if err != nil {
		return ""
	}
	var ids []string
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok || (k != "ID" && k != "ID_LIKE") {
			continue
		}
		ids = append(ids, strings.Fields(strings.ToLower(strings.Trim(v, `"'`)))...)
	}
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "debian"
		case "fedora", "rhel", "centos":
			return "fedora"
		case "arch":
			return "arch"
		}
	}
	return ""
}

// mediaInstall is the install command for this OS. fixable is true only
// when --fix may run it: macOS with brew on PATH (no sudo).
//
// --fix does NOT run the sudo (Linux) or winget (Windows) commands: the
// user has not decided whether it may. They are printed as the fix. To
// allow it later, add a case for the "sudo " and "winget " prefixes in
// fixable() (doctor.go): one line.
func mediaInstall() (cmd string, fixable bool) {
	switch osFamily() {
	case "darwin":
		if _, err := mediaLookPath("brew"); err == nil {
			return "brew install " + mediaPkgs, true
		}
		return "install Homebrew (https://brew.sh), then: brew install " + mediaPkgs, false
	case "debian":
		return "sudo apt install " + mediaPkgs, false
	case "fedora":
		return "sudo dnf install ImageMagick ffmpeg (ffmpeg may need RPM Fusion enabled first)", false
	case "arch":
		return "sudo pacman -S " + mediaPkgs, false
	case "windows":
		return "winget install ImageMagick.ImageMagick Gyan.FFmpeg", false
	}
	return "install ImageMagick and ffmpeg with your package manager", false
}

// mediaTools probes in agentkit's order.
func mediaTools() (magick, convert, ffmpeg, vips mediaTool) {
	magick = mediaProbe("magick", "-version")
	if mediaGOOS != "windows" {
		convert = mediaProbe("convert", "-version")
	}
	ffmpeg = mediaProbe("ffmpeg", "-version")
	vips = mediaProbe("vipsthumbnail", "--version")
	return
}

func (t mediaTool) say() string {
	return fmt.Sprintf("%s %s (%s)", t.name, t.path, t.version)
}

// media reports the scaling tools agentkit finds, which it uses, and what
// is missing. Never a fail: the stdlib path still scales png, jpeg, gif.
func (d *doctor) media() {
	magick, convert, ffmpeg, vips := mediaTools()
	install, _ := mediaInstall()

	if convert.path != "" && !convert.usable {
		d.add("warn", "media", fmt.Sprintf("convert at %s is not ImageMagick (its -version does not say so); agentkit may run it and fail", convert.path),
			"install ImageMagick 7 so magick is found first: "+install)
	}
	var use *mediaTool
	for _, t := range []*mediaTool{&magick, &convert, &ffmpeg, &vips} {
		if t.path != "" && t.usable {
			use = t
			break
		}
	}
	for _, t := range []mediaTool{magick, convert, ffmpeg, vips} {
		if t.path != "" && t.usable {
			d.add("ok", "media", t.say(), "")
		}
	}
	if use == nil {
		d.add("warn", "media", "no image tool (magick, convert, ffmpeg, vipsthumbnail): images over the cap and WebP/HEIC can't be scaled; install ImageMagick; only png, jpeg, gif by the built-in path", install)
	} else {
		d.add("ok", "media", "images scale with "+use.name+"; the other tools are fallbacks", "")
	}
	if ffmpeg.path == "" {
		fix := ""
		if use != nil { // the image warning carries the fix otherwise
			fix = install
		}
		d.add("warn", "media", "no ffmpeg: audio and video can't be scaled (only ffmpeg does it)", fix)
	} else {
		d.add("ok", "media", "audio and video scale with ffmpeg", "")
	}
}
