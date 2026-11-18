package dund

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iodesystems/dun/proto"
)

// A peer request that changes something (a message to another session, a
// schedule change) is done once, however often it comes: an engine sends
// its open requests again when a dund attaches (cmd/dun/peers.go resend),
// and a dund that crashed before its ack gets the replay of ones it did
// (docs/engine-adoption.md). Its id is unique beyond the engine (pid and
// start); dund keeps the recent ones it took, in <dir>/peer-done.json, so a
// restart remembers them. A copy of one done gets the first answer; a copy
// of one a restart cut while it was being done is told so, not done again.

// peerDoneMax is how many requests are remembered.
const peerDoneMax = 256

type peerDoneEntry struct {
	Key  string           `json:"key"` // session and request id
	Done bool             `json:"done,omitempty"`
	Rep  *proto.PeerReply `json:"rep,omitempty"`

	wait chan struct{} // in this process: closed once Done
}

type peerDone struct {
	mu      sync.Mutex
	path    string
	entries []*peerDoneEntry // oldest first
}

func loadPeerDone(dir string) *peerDone {
	d := &peerDone{path: filepath.Join(dir, "peer-done.json")}
	if b, err := os.ReadFile(d.path); err == nil {
		if err := json.Unmarshal(b, &d.entries); err != nil {
			log.Printf("dund: %s: %v; starting empty", d.path, err)
			d.entries = nil
		}
	}
	return d
}

// mutating: ops whose repeat would do a thing twice. list, oauth and the
// gateway's share are asked again freely (a share is re-granted on the new
// dund's gateway, which is what an engine needs after a restart).
func mutating(op string) bool { return op == "msg" || op == "sched" }

// once runs do for req, from session ses, unless it was taken before: then
// the first answer, or an error that says it may have been done.
func (d *peerDone) once(ses string, req proto.PeerRequest, do func() proto.PeerReply) proto.PeerReply {
	// An id of an engine before unique ids ("p3") repeats across engines.
	if d == nil || !mutating(req.Op) || !strings.Contains(req.RID, ".") {
		return do()
	}
	key := ses + "\x00" + req.RID
	d.mu.Lock()
	for _, e := range d.entries {
		if e.Key != key {
			continue
		}
		wait := e.wait
		d.mu.Unlock()
		if wait != nil {
			<-wait // the first copy runs in this process
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if e.Done && e.Rep != nil {
			rep := *e.Rep
			return rep
		}
		return proto.PeerReply{RID: req.RID, Error: "dund restarted while it did this request: " +
			"it may or may not have been done; check before you try again"}
	}
	e := &peerDoneEntry{Key: key, wait: make(chan struct{})}
	d.entries = append(d.entries, e)
	if len(d.entries) > peerDoneMax {
		d.entries = d.entries[len(d.entries)-peerDoneMax:]
	}
	d.saveLocked() // taken, before it is done: a crash now is not done twice
	d.mu.Unlock()
	rep := do()
	d.mu.Lock()
	e.Done, e.Rep = true, &rep
	d.saveLocked()
	close(e.wait)
	e.wait = nil
	d.mu.Unlock()
	return rep
}

func (d *peerDone) saveLocked() {
	b, err := json.Marshal(d.entries)
	if err == nil {
		err = writeFileAtomic(d.path, b)
	}
	if err != nil {
		log.Printf("dund: %s: %v", d.path, err)
	}
}
