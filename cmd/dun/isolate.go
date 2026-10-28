package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// /isolate: where the session's files and commands are (plan, "Workspace and
// tools"). The same words as a sub-agent's isolation (dun/subisolation.go):
//
//	none, off  in place: the checkout itself, commands on this machine
//	worktree   a fresh git worktree, commands on this machine
//	docker     a worktree AND its commands in a container on it
//
// docker always brings a worktree. A container mounted on the checkout runs
// commands elsewhere but edits the developer's own files, which is no
// isolation of the files. /docker and /worktree new are the old words for
// two of these, kept as aliases that say so (isolateAlias).
//
// Nothing here deletes a worktree. off leaves it on disk and says where; the
// startup prune removes one that holds no work, and keeps one that does.

// dockerProbe says whether Docker can be used. A var so tests can say no (and
// yes) without depending on the machine.
var dockerProbe = func() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed (not on PATH)")
	}
	return nil
}

// isolateChoices are /isolate's values, for the palette and /help.
var isolateChoices = choices(
	dun.IsoNone, "in place: the checkout itself, commands on this machine",
	dun.IsoWorktree, "a fresh git worktree on a new branch; commands on this machine",
	dun.IsoDocker, "a fresh git worktree, its commands in a Docker container on it",
	"off", "back in place, as none; a worktree is kept on disk, never deleted")

// isolationMode is the session's isolation now: none, worktree or docker.
func isolationMode(h *dun.Harness) string {
	if wt := h.Worktree(); wt != nil && wt.Branch != "" {
		if h.IsDocker() {
			return dun.IsoDocker
		}
		return dun.IsoWorktree
	}
	if h.IsDocker() {
		// Docker on the checkout: only an old /docker on could do this, or
		// a session resumed from one. Reported as what it is.
		return dun.IsoDocker
	}
	return dun.IsoNone
}

// runIsolateCmd handles /isolate [none|worktree|docker|off] and
// /isolate docker image [<image | Dockerfile | directory>].
func runIsolateCmd(h *dun.Harness, action string) string {
	if rest, ok := strings.CutPrefix(action, "docker image"); ok && (rest == "" || rest[0] == ' ') {
		return isolateImage(h, strings.TrimSpace(rest))
	}
	switch action {
	case "", "status":
		return isolateStatus(h) + "\n" + isolateUsage
	case dun.IsoNone, "off":
		return isolateOff(h)
	case dun.IsoWorktree:
		return isolateWorktree(h, false)
	case dun.IsoDocker:
		return isolateWorktree(h, true)
	default:
		return "isolate: unknown " + strconv.Quote(action) + "\n" + isolateUsage
	}
}

const isolateUsage = "/isolate worktree: a fresh git worktree · /isolate docker: a worktree, commands in Docker · " +
	"/isolate docker image <image | Dockerfile>: the image for docker · /isolate off: back in place"

// Image tags dun builds from a Dockerfile: dun-iso-<12 hex of the file's sha256>.
const isoImagePrefix = "dun-iso-"

// dockerImageExists and dockerBuild run docker. Vars so tests need no docker.
var dockerImageExists = func(tag string) bool {
	return exec.Command("docker", "image", "inspect", tag).Run() == nil
}

var dockerBuild = func(tag, dockerfile, contextDir string) (string, error) {
	out, err := exec.Command("docker", "build", "-t", tag, "-f", dockerfile, contextDir).CombinedOutput()
	return string(out), err
}

// imageArg is what /isolate docker image was given: an image to use as is, or
// a Dockerfile to build (Tag is then its deterministic tag).
type imageArg struct {
	Image      string // as given, when nothing is built
	Dockerfile string // absolute path; "" when Image is used as is
	Context    string // build context: the Dockerfile's directory
	Tag        string
}

// parseImageArg decides what arg is. An existing file is a Dockerfile; a
// directory must hold one; a path-looking word (starts with . / ~) that does
// not exist is an error, not an image name; anything else is an image.
func parseImageArg(arg, base string) (imageArg, error) {
	if strings.HasPrefix(arg, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			arg = filepath.Join(home, arg[2:])
		}
	}
	path := arg
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.IsDir():
		df := filepath.Join(path, "Dockerfile")
		if st, err := os.Stat(df); err != nil || st.IsDir() {
			return imageArg{}, fmt.Errorf("%s is a directory with no Dockerfile in it", path)
		}
		path = df
	case err == nil:
	case strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~"):
		return imageArg{}, fmt.Errorf("%s does not exist", path)
	case errors.Is(err, os.ErrNotExist):
		return imageArg{Image: arg}, nil
	default:
		return imageArg{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return imageArg{}, err
	}
	return imageArg{Dockerfile: path, Context: filepath.Dir(path), Tag: isoImageTag(data)}, nil
}

// isoImageTag is the same for the same Dockerfile text.
func isoImageTag(dockerfile []byte) string {
	sum := sha256.Sum256(dockerfile)
	return isoImagePrefix + hex.EncodeToString(sum[:])[:12]
}

// isolateBuilds reports whether a control command may run a long docker build;
// like one that asks, it must not run on the reader goroutine.
func isolateBuilds(id, action string) bool {
	return id == "isolate" && strings.HasPrefix(action, "docker image ")
}

// isolateImage handles /isolate docker image [arg].
func isolateImage(h *dun.Harness, arg string) string {
	if arg == "" {
		return "isolate: docker image " + h.DockerImage()
	}
	a, err := parseImageArg(arg, h.Workspace())
	if err != nil {
		return "isolate: docker image refused — " + err.Error()
	}
	if a.Dockerfile == "" {
		h.SetDockerImage(a.Image)
		return "isolate: docker image " + a.Image + "\n" + imageNote(h)
	}
	if err := dockerProbe(); err != nil {
		return "isolate: docker image refused — " + err.Error()
	}
	if dockerImageExists(a.Tag) {
		h.SetDockerImage(a.Tag)
		return "isolate: docker image " + a.Tag + " (built before from " + a.Dockerfile + ")\n" + imageNote(h)
	}
	if out, err := dockerBuild(a.Tag, a.Dockerfile, a.Context); err != nil {
		return "isolate: docker build of " + a.Dockerfile + " failed — " + oneLine(err.Error()) + "\n" + tailLines(out, 15)
	}
	h.SetDockerImage(a.Tag)
	return "isolate: docker image " + a.Tag + " (built from " + a.Dockerfile + ")\n" + imageNote(h)
}

// imageNote says what the new image affects now.
func imageNote(h *dun.Harness) string {
	if h.IsDocker() {
		return "commands now run in it"
	}
	return "used by /isolate docker and by sub-agents with isolation docker"
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// isolateStatus is one line: where the files are and where commands run.
func isolateStatus(h *dun.Harness) string {
	wt := h.Worktree()
	dedicated := wt != nil && wt.Branch != ""
	where := "in place: " + h.Workspace()
	if dedicated {
		where = fmt.Sprintf("worktree: %s (branch %s)", wt.Path, wt.Branch)
	}
	if h.IsDocker() {
		if !dedicated {
			where += ", files not isolated"
		}
		return "isolate: docker — " + where + "; commands in a container (image " + dockerImage(h) + ")"
	}
	mode := dun.IsoNone
	if dedicated {
		mode = dun.IsoWorktree
	}
	return "isolate: " + mode + " — " + where + "; commands on this machine"
}

func dockerImage(h *dun.Harness) string {
	if d, ok := h.ExecBackend().(dun.DockerExec); ok {
		return d.Image
	}
	return h.DockerImage()
}

// isolateWorktree moves the session into a worktree (a fresh one unless it is
// in one already) and sets where commands run: Docker or this machine.
func isolateWorktree(h *dun.Harness, docker bool) string {
	mode := dun.IsoWorktree
	if docker {
		mode = dun.IsoDocker
	}
	if wt := h.Worktree(); wt != nil && wt.Branch != "" && h.IsDocker() == docker {
		return "isolate: already " + mode + "\n" + isolateStatus(h)
	}
	if h.NoDir() {
		return "isolate: " + mode + " refused — a --no-dir session has no project to put in a worktree"
	}
	if docker {
		if err := dockerProbe(); err != nil {
			return "isolate: docker refused — " + err.Error() + ". /isolate worktree isolates the files without it"
		}
	}
	if wt := h.Worktree(); wt != nil && wt.Branch != "" {
		// In a worktree already: keep it, change only where commands run.
		h.Rehoist(wt.Path, wt, docker)
		return "isolate: " + mode + " — kept the worktree\n" + isolateStatus(h)
	}
	if _, isRepo := dun.WorktreeInPlace(h.Workspace()); !isRepo {
		return "isolate: " + mode + " refused — " + h.Workspace() + " is not in a git repository, and a worktree needs one"
	}
	// The mounts are the session's, not nil. A worktree lives under
	// $DUN_HOME/projects/<id>/worktrees/, so a go.mod "replace =>
	// ../agentkit" resolves through a symlink NewWorktree puts beside it;
	// with nil, the first build inside a worktree made mid-session failed
	// on a dependency that resolves fine at startup. The same list Docker
	// mounts, so both see one set of paths.
	newWt, isRepo, err := dun.NewWorktree(h.Workspace(), h.Mounts())
	if err != nil {
		return "isolate: " + mode + " failed — " + oneLine(err.Error())
	}
	if !isRepo {
		return "isolate: " + mode + " refused — " + h.Workspace() + " is not in a git repository, and a worktree needs one"
	}
	h.Rehoist(newWt.Path, newWt, docker)
	return "isolate: " + mode + " — created the worktree\n" + isolateStatus(h)
}

// isolateOff goes back to the checkout, commands on this machine. A worktree
// the session leaves stays on disk, and the reply says where.
func isolateOff(h *dun.Harness) string {
	if isolationMode(h) == dun.IsoNone {
		return "isolate: already in place\n" + isolateStatus(h)
	}
	left := h.Worktree()
	home := h.Home()
	inPlace, _ := dun.WorktreeInPlace(home)
	h.Rehoist(home, inPlace, false)
	msg := "isolate: off — " + strings.TrimPrefix(isolateStatus(h), "isolate: none — ")
	if left != nil && left.Branch != "" {
		msg += fmt.Sprintf("\nthe worktree is kept: %s (branch %s). Merge the branch, or remove it with: git worktree remove %s; "+
			"one with no changes and no commits is removed when dun next starts", left.Path, left.Branch, left.Path)
	}
	return msg
}

// isolateEvents are what a control command that changed the isolation tells
// every client: the command list (/isolate's state), and, when the session
// moved, where it is now, as at start, so dund's dun_list shows the worktree
// and every client the branch. Back in place, the branch is empty.
func isolateEvents(h *dun.Harness, wsBefore, isoBefore string) []any {
	var out []any
	if isolationMode(h) != isoBefore {
		out = append(out, commandsEvent(h))
	}
	if ws := h.Workspace(); ws != wsBefore {
		branch := ""
		if wt := h.Worktree(); wt != nil {
			branch = wt.Branch
		}
		out = append(out, &proto.Workspace{Path: ws, Branch: branch})
	}
	return out
}

// isolateAlias is /docker and /worktree new, the old words: what /isolate
// action it is, with the one line that names it. ok false: not an alias
// (/worktree status and commit are their own commands).
func isolateAlias(id, action string) (iso, note string, ok bool) {
	switch {
	case id == "docker" && (action == "" || action == "status"):
		return "", "/docker is /isolate now", true
	case id == "docker" && action == "on":
		return dun.IsoDocker, "/docker on is /isolate docker now: a worktree, its commands in Docker", true
	case id == "docker" && action == "off":
		return "off", "/docker off is /isolate off now", true
	case id == "worktree" && action == "new":
		return dun.IsoWorktree, "/worktree new is /isolate worktree now", true
	}
	return "", "", false
}
