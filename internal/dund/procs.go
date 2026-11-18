package dund

import (
	"os"
	"sort"
	"sync"

	"github.com/iodesystems/dun/internal/harness"

	"github.com/iodesystems/dun/proto"
)

// PID is the engine's process id, for dun ps.
func (e *execEngine) PID() int {
	if e.cmd.Process == nil {
		return 0
	}
	return e.cmd.Process.Pid
}

// pider is an Engine that knows its process id (the real one; test fakes
// do not).
type pider interface{ PID() int }

// Procs answers dun ps: principal's running sessions, each with its
// engine's pid, the trees it and its sub-agents work in, and its jobs'
// process groups. The client reads the process table itself
// (internal/procps).
func (s *Service) Procs(principal string) *proto.ProcsList {
	s.mu.Lock()
	var mine []*session
	for _, ses := range s.sessions {
		if ses.isMember(principal) {
			mine = append(mine, ses)
		}
	}
	s.mu.Unlock()
	out := &proto.ProcsList{DundPID: os.Getpid(), Sessions: []proto.ProcSession{}}
	for _, ses := range mine {
		ses.mu.Lock()
		running, eng := ses.running, ses.eng
		ses.mu.Unlock()
		ses.actMu.Lock()
		bpid, bwhy := ses.blockPid, ses.blockWhy
		ses.actMu.Unlock()
		if (!running || eng == nil) && bpid != 0 {
			// An engine from before dund's restart that was not adopted and
			// may still run: shown, for a person to end it.
			out.Sessions = append(out.Sessions, proto.ProcSession{ID: ses.id, Workspace: ses.workspace,
				Project: projectSlug(ses.workspace), EnginePID: bpid, Blocked: bwhy})
			continue
		}
		if !running || eng == nil {
			continue
		}
		ps := proto.ProcSession{ID: ses.id, Workspace: ses.workspace, Project: projectSlug(ses.workspace)}
		if p, ok := eng.(pider); ok {
			ps.EnginePID = p.PID()
		}
		ses.actMu.Lock()
		ps.Worktree = ses.worktree
		ps.Agents = append(ps.Agents, ses.agentDirs...)
		for _, j := range ses.jobs {
			ps.Jobs = append(ps.Jobs, proto.ProcJob{ID: j.ID, Command: j.Command, PGID: j.PGID})
		}
		ses.actMu.Unlock()
		out.Sessions = append(out.Sessions, ps)
	}
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].ID < out.Sessions[j].ID })
	return out
}

// projectSlugs caches harness.ProjectSlug by workspace: it runs git, and
// dun ps --watch asks every interval.
var projectSlugs sync.Map

// projectSlug is the rag project slug of workspace (the key of its indexes
// in ragd), for dun ps to give a session its project's indexes.
func projectSlug(workspace string) string {
	if workspace == "" {
		return ""
	}
	if v, ok := projectSlugs.Load(workspace); ok {
		return v.(string)
	}
	slug := harness.ProjectSlug(workspace)
	projectSlugs.Store(workspace, slug)
	return slug
}
