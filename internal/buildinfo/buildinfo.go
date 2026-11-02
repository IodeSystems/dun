// Package buildinfo says which build this is and where it came from. The
// -X stamps stay on package main (main.version, main.channel, main.srcDir are
// what update.FromBuildInfo, the Makefile and the self-rebuild read); main
// copies them here before anything runs.
package buildinfo

import (
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// Version is main.version.
var Version = "dev"

// Channel is main.channel.
var Channel = ""

// IsRelease: stamped release AND built with the tag that carries the
// manifest's location and the key.
func IsRelease() bool { return Channel == "release" && release.Built }

// SelfOrigin is how this binary was built.
func SelfOrigin() update.Origin {
	o := update.Origin{Kind: update.GoBuild}
	if bi, ok := debug.ReadBuildInfo(); ok {
		o = update.FromBuildInfo(bi)
	}
	switch {
	case Channel == "release":
		o.Kind, o.Version = update.Release, Version
	case SrcDir != "":
		o.Kind = update.Checkout
		if o.Revision == "" {
			o.Revision = Version
		}
	}
	return o
}

// SrcDir is main.srcDir.
var SrcDir = ""

// SelfExe is this binary's file, symlinks resolved.
func SelfExe() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return exe, err
}
