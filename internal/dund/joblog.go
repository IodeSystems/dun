package dund

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/iodesystems/dun/proto"
)

// JobOutputMax is the most JobOutput returns in one read.
const JobOutputMax = 256 << 10

// JobOutput is part of a background job's output log, for a client that
// cannot read the file (the web client, a phone).
type JobOutput struct {
	Job    int    `json:"job"`
	State  string `json:"state"`
	Offset int64  `json:"offset"` // where Data starts in the log
	Size   int64  `json:"size"`   // the log's size when read
	Next   int64  `json:"next"`   // where this read ended: the next since
	Data   string `json:"data"`
}

// JobOutput reads session id's job job's log: since >= 0 from that offset,
// otherwise the last tail bytes. At most limit (JobOutputMax when 0 or more)
// bytes. Only a log the engine named in the session's last jobs event
// is read, so a caller cannot name a path; the session's membership check is
// Get's.
func (s *Service) JobOutput(principal, id string, job int, since, tail int64, limit int) (*JobOutput, error) {
	ses, err := s.Get(principal, id)
	if err != nil {
		return nil, err
	}
	out := &JobOutput{Job: job}
	path := ""
	// The log's last jobs event, not ses.jobs: that keeps the running ones
	// only, and a finished job's output is what a person opens most.
	for _, ev := range ses.log.State() {
		if p, ok := ev.Payload.(*proto.Jobs); ok {
			for _, j := range p.Jobs {
				if j.ID == job {
					path, out.State = j.Log, j.State
				}
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("job #%d has no output here", job)
	}
	if limit <= 0 || limit > JobOutputMax {
		limit = JobOutputMax
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil // not written yet
	}
	if err != nil {
		return nil, fmt.Errorf("job #%d output: %w", job, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("job #%d output is not a file", job)
	}
	out.Size = st.Size()
	switch {
	case since >= 0:
		out.Offset = min(since, out.Size)
	case tail > 0:
		out.Offset = max(out.Size-min(tail, int64(limit)), 0)
	}
	n := min(out.Size-out.Offset, int64(limit))
	buf := make([]byte, n)
	k, err := f.ReadAt(buf, out.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("job #%d output: %w", job, err)
	}
	out.Data = string(buf[:k])
	out.Next = out.Offset + int64(k)
	return out, nil
}
