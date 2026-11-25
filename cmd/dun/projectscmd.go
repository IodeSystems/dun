package main

import (
	"fmt"
	"io"
	"os"

	"github.com/iodesystems/dun/internal/project"
)

// `dun projects`: manage the project registry.
//
// The registry maps an absolute source directory to a project id. Sessions
// and memory are keyed by that id, so moving a project (re-point) keeps its
// state: the id is what files are named after.
//
//	dun projects             list registered projects
//	dun projects forget <id> remove the entry (state left in place)
//	dun projects re-point <id> <new-path>  move an entry to a new path

const projectsHelp = `dun projects — the project registry

  dun projects                 list registered projects (id, path)
  dun projects forget <id>     remove a registry entry (sessions and memory stay)
  dun projects re-point <id> <new-path>
                               move an entry's path to a new location

The registry maps a directory to a project id. Sessions and memory are keyed
by the id, so re-pointing a project to a new path keeps its state: the old
path simply stops resolving to the id and the new one starts.

A project whose path no longer exists is marked (moved).`

func projectsCmd(args []string, out, errw io.Writer) int {
	switch {
	case len(args) == 0:
		return projectsList(out)
	case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Fprint(out, projectsHelp)
		return 0
	case args[0] == "forget" && len(args) == 2:
		return projectsForget(args[1], out, errw)
	case args[0] == "re-point" && len(args) == 3:
		return projectsRepoint(args[1], args[2], out, errw)
	case len(args) == 1 && (args[0] == "forget" || args[0] == "re-point"):
		fmt.Fprintln(errw, "dun projects: missing argument")
		fmt.Fprint(errw, projectsHelp)
		return 2
	default:
		fmt.Fprintln(errw, "dun projects: unknown subcommand")
		fmt.Fprint(errw, projectsHelp)
		return 2
	}
}

func projectsList(out io.Writer) int {
	entries, err := project.List()
	if err != nil {
		fmt.Fprintln(out, "dun projects:", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "no projects registered")
		return 0
	}
	for _, e := range entries {
		if _, err := os.Stat(e.Source); os.IsNotExist(err) {
			fmt.Fprintf(out, "%s  %s  (moved)\n", e.ID, e.Source)
		} else {
			fmt.Fprintf(out, "%s  %s\n", e.ID, e.Source)
		}
	}
	return 0
}

func projectsForget(id string, out, errw io.Writer) int {
	if err := project.Forget(id); err != nil {
		fmt.Fprintln(errw, "dun projects forget:", err)
		return 1
	}
	fmt.Fprintf(out, "forgotten %s (state left in place)\n", id)
	return 0
}

func projectsRepoint(id, newPath string, out, errw io.Writer) int {
	if err := project.Repoint(id, newPath); err != nil {
		fmt.Fprintln(errw, "dun projects re-point:", err)
		return 1
	}
	fmt.Fprintf(out, "re-pointed %s → %s\n", id, project.RealPath(newPath))
	return 0
}
