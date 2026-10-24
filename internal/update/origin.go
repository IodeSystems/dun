package update

import (
	"debug/buildinfo"
	"runtime/debug"
	"slices"
	"strings"
)

// Where a binary came from decides how it updates: a release build from the
// manifest, a `go install` build with go install …@latest, a build from a
// checkout by rebuilding it. The Go toolchain records enough in every binary
// to tell them apart without running it (debug/buildinfo):
//
//   - release: built with -tags release (release builds also use -trimpath,
//     which drops -ldflags from the record, so the tag is the mark).
//   - checkout: vcs.revision is set; `go build` in a repository stamps it.
//   - go install: a module version and no vcs settings — built from the
//     module zip, as `go install module@version` does.
//   - go build: none of these (a build outside version control).

const (
	Release   = "release"
	Checkout  = "checkout"
	GoInstall = "go install"
	GoBuild   = "go build"
)

// Origin is how a binary was built.
type Origin struct {
	Kind     string
	Module   string // the main module's path
	Version  string // the main module's version ("(devel)" for most local builds)
	Revision string // vcs.revision, shortened
	Modified bool   // vcs.modified
	CGO      bool   // CGO_ENABLED=1
	SrcDir   string // -X main.srcDir=…: the checkout a make install build came from
}

// FromBuildInfo reads an Origin from a binary's build info.
func FromBuildInfo(bi *debug.BuildInfo) Origin {
	o := Origin{Kind: GoBuild, Module: bi.Main.Path, Version: bi.Main.Version}
	release := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			o.Revision = s.Value
			if len(o.Revision) > 7 {
				o.Revision = o.Revision[:7]
			}
		case "vcs.modified":
			o.Modified = s.Value == "true"
		case "CGO_ENABLED":
			o.CGO = s.Value == "1"
		case "-tags":
			release = release || slices.Contains(strings.Split(s.Value, ","), "release")
		case "-ldflags":
			release = release || strings.Contains(s.Value, "main.channel=release")
			o.SrcDir = ldflagX(s.Value, "main.srcDir")
		}
	}
	switch {
	case release:
		o.Kind = Release
	case o.Revision != "":
		o.Kind = Checkout
	case o.Version != "" && o.Version != "(devel)":
		o.Kind = GoInstall
	}
	return o
}

// ldflagX is the value -X name=value sets in an -ldflags string, "" when
// none. The string is recorded as it was given, so a value with spaces is
// quoted ("-X 'main.srcDir=/a b'"): it is split as the go command splits it,
// on spaces outside single or double quotes.
func ldflagX(flags, name string) string {
	var args []string
	var cur strings.Builder
	in, q := false, byte(0)
	for i := 0; i < len(flags); i++ {
		c := flags[i]
		switch {
		case q != 0 && c == q:
			q = 0
		case q != 0:
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			q, in = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				args, in = append(args, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		args = append(args, cur.String())
	}
	for i, a := range args {
		switch {
		case (a == "-X" || a == "--X") && i+1 < len(args):
			a = args[i+1]
		case strings.HasPrefix(a, "-X="):
			a = a[len("-X="):]
		case strings.HasPrefix(a, "-X "):
			a = a[len("-X "):]
		default:
			continue
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// OfFile reads the Origin of the binary at path, without running it.
func OfFile(path string) (Origin, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return Origin{}, err
	}
	return FromBuildInfo(bi), nil
}

// String says it in doctor's words: "release v0.3.0", "go install v0.9.2
// (cgo)", "built from a checkout at a019fc7, modified".
func (o Origin) String() string {
	var s string
	switch o.Kind {
	case Release:
		s = "release " + o.Version
	case GoInstall:
		s = "go install " + o.Version
	case Checkout:
		s = "built from a checkout at " + o.Revision
		if o.Modified {
			s += ", modified"
		}
	default:
		s = "a plain go build, outside version control"
	}
	if o.CGO {
		s += " (cgo)"
	}
	return s
}
