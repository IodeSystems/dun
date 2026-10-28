package dund

import (
	"fmt"

	"github.com/iodesystems/dun/proto"
)

// JobUsage answers a client's job_usage: what session id's background job
// job is doing, sampled from its process group (internal/procstat). dund
// runs where the jobs run and every client talks to it, so a client asks
// here; the answer is not logged (USER 2026-10-28: "these numbers would be
// checked on by dund"). The group is the one the engine named in its last
// jobs event.
func (s *Service) JobUsage(principal, id string, job int) *proto.JobUsage {
	out := &proto.JobUsage{Job: job}
	ses, err := s.Get(principal, id)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	pgid := 0
	ses.actMu.Lock() // what track writes jobs under
	for _, j := range ses.jobs {
		if j.ID == job {
			pgid = j.PGID
		}
	}
	ses.actMu.Unlock()
	if pgid == 0 {
		out.Error = fmt.Sprintf("job #%d is not running here, or its processes cannot be read (a docker job)", job)
		return out
	}
	out.Usage = s.procs.Sample(pgid)
	out.Verdict = out.Usage.Verdict(0)
	return out
}
