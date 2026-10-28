package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

func TestParseImageArg(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(dir, "my.Dockerfile")
	if err := os.WriteFile(named, []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	// An image name, with and without a registry, is used as is.
	for _, img := range []string{"ubuntu:24.04", "ghcr.io/x/y:1", "golang"} {
		a, err := parseImageArg(img, dir)
		if err != nil || a.Image != img || a.Dockerfile != "" {
			t.Errorf("%s: %+v, %v", img, a, err)
		}
	}
	// A file, a relative file, and a directory holding one: all built.
	for _, arg := range []string{df, "Dockerfile", "my.Dockerfile", dir} {
		a, err := parseImageArg(arg, dir)
		if err != nil || a.Dockerfile == "" || a.Context != dir || a.Image != "" {
			t.Errorf("%s: %+v, %v", arg, a, err)
		}
		if !strings.HasPrefix(a.Tag, isoImagePrefix) {
			t.Errorf("%s: tag %q", arg, a.Tag)
		}
	}
	// Errors: no Dockerfile in the directory; a path that is not there.
	for _, arg := range []string{empty, "./nope", "/no/such/Dockerfile"} {
		if a, err := parseImageArg(arg, dir); err == nil {
			t.Errorf("%s: accepted as %+v", arg, a)
		}
	}
}

func TestIsoImageTag_Deterministic(t *testing.T) {
	a, b := isoImageTag([]byte("FROM a\n")), isoImageTag([]byte("FROM a\n"))
	if a != b || len(a) != len(isoImagePrefix)+12 {
		t.Errorf("%q %q", a, b)
	}
	if a == isoImageTag([]byte("FROM b\n")) {
		t.Error("different Dockerfiles, one tag")
	}
}

// stubDocker replaces the docker runners; the result lists the tags built.
func stubDocker(t *testing.T, exists bool, buildErr error) *[]string {
	t.Helper()
	var built []string
	oldE, oldB := dockerImageExists, dockerBuild
	dockerImageExists = func(string) bool { return exists }
	dockerBuild = func(tag, _, _ string) (string, error) {
		built = append(built, tag)
		return "step 1\nstep 2\n", buildErr
	}
	t.Cleanup(func() { dockerImageExists, dockerBuild = oldE, oldB })
	return &built
}

func TestIsolateImage(t *testing.T) {
	dockerAvailable(t, nil)
	repo := isoRepo(t)
	h := isoHarness(t, repo)
	if got := ctl(h, "isolate", "docker image"); !strings.HasSuffix(got, dun.DefaultIsolationImage) {
		t.Errorf("default: %q", got)
	}
	built := stubDocker(t, false, nil)
	if got := ctl(h, "isolate", "docker image alpine:3"); !strings.Contains(got, "alpine:3") || len(*built) != 0 {
		t.Errorf("plain image: %q built %v", got, *built)
	}
	if h.DockerImage() != "alpine:3" {
		t.Errorf("image %q", h.DockerImage())
	}
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ctl(h, "isolate", "docker image .")
	if len(*built) != 1 || h.DockerImage() != (*built)[0] || !strings.Contains(got, "built from") {
		t.Errorf("build: %q built %v image %q", got, *built, h.DockerImage())
	}
	tag := h.DockerImage()
	// The tag exists: no second build.
	built2 := stubDocker(t, true, nil)
	ctl(h, "isolate", "docker image alpine:3")
	ctl(h, "isolate", "docker image Dockerfile")
	if len(*built2) != 0 || h.DockerImage() != tag {
		t.Errorf("rebuilt: %v image %q", *built2, h.DockerImage())
	}
	// A failed build keeps the old image and shows the output's tail.
	stubDocker(t, false, errors.New("exit status 1"))
	ctl(h, "isolate", "docker image alpine:3")
	got = ctl(h, "isolate", "docker image Dockerfile")
	if !strings.Contains(got, "failed") || !strings.Contains(got, "step 2") || h.DockerImage() != "alpine:3" {
		t.Errorf("failed build: %q image %q", got, h.DockerImage())
	}
}

// A session already in Docker switches image at once.
func TestIsolateImage_Live(t *testing.T) {
	dockerAvailable(t, nil)
	h := isoHarness(t, isoRepo(t))
	ctl(h, "isolate", "docker")
	ctl(h, "isolate", "docker image alpine:3")
	if d, ok := h.ExecBackend().(dun.DockerExec); !ok || d.Image != "alpine:3" {
		t.Errorf("backend %+v", h.ExecBackend())
	}
	if !strings.Contains(ctl(h, "isolate", ""), "image alpine:3") {
		t.Error("status does not name the image")
	}
}

func TestCheckDockerFlag(t *testing.T) {
	for _, ok := range []string{"", resolveDockerImage("true"), "ubuntu:24.04"} {
		if err := checkDockerFlag(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	err := checkDockerFlag("my/img:1")
	if err == nil || !strings.Contains(err.Error(), "/isolate docker image my/img:1") {
		t.Errorf("custom image: %v", err)
	}
}

func TestIsolateBuilds(t *testing.T) {
	if !isolateBuilds("isolate", "docker image x") || isolateBuilds("isolate", "docker image") || isolateBuilds("isolate", "docker") {
		t.Error("isolateBuilds")
	}
}
