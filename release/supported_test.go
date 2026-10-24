package release_test

import (
	"os"
	"testing"

	"golang.org/x/mod/semver"

	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// Every supported version is one the proxy can resolve (not "latest"), and
// the minimum is not past the recommended.
func TestSupportedVersions(t *testing.T) {
	for _, s := range release.Supported {
		if !semver.IsValid(s.Min) || !semver.IsValid(s.Recommended) {
			t.Errorf("%s: min %q, recommended %q: each must be a module version", s.Name, s.Min, s.Recommended)
		}
		if semver.Compare(s.Min, s.Recommended) > 0 {
			t.Errorf("%s: min %s is past recommended %s", s.Name, s.Min, s.Recommended)
		}
	}
}

// A release build's pinned tools (deps.json) are the versions this dun
// supports: --fix downloads them in a release build and go-installs them in
// any other, so the two must not differ. An empty deps.json (no tools
// published yet) pins nothing.
func TestDepsAgreeWithSupported(t *testing.T) {
	b, err := os.ReadFile("deps.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := update.ParseManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range m.Artifacts {
		s, ok := release.SupportedTool(name)
		if !ok {
			t.Errorf("deps.json pins %s, which release.Supported does not list", name)
			continue
		}
		if a.Version != s.Recommended {
			t.Errorf("deps.json pins %s %s; release.Supported recommends %s", name, a.Version, s.Recommended)
		}
	}
}
