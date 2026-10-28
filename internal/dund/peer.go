package dund

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// dun↔dun messaging: the dund half of dun_list and dun_msg (proto/peer.go).
// A session reaches only its owner's sessions, and only the ones loaded in a
// running engine: a session merely resumable from disk is neither listed nor
// started to take a message.

// peerRateMax messages per peerRateWindow, per (sender, target): two sessions
// that wake each other with every reply would otherwise loop for as long as
// the provider pays.
const (
	peerRateMax    = 6
	peerRateWindow = time.Minute
)

// peerTaskMax clips a listed task line.
const peerTaskMax = 120

// sessionTask is the session's status or goal from its state sidecar (a
// variable: the tests' fake engine writes none, and the real one is under the
// user's dun home).
var sessionTask = func(workspace, id string) string {
	st := dun.LoadSessionState(dun.SessionFile(workspace, id))
	if st.Status != "" {
		return st.Status
	}
	return st.Goal
}

// handlePeer answers an engine's peer request. Its own goroutine, never the
// event reader's: delivering writes to another engine's stdin, and two
// readers each waiting on the other's engine would deadlock.
func (s *Service) handlePeer(from *session, req proto.PeerRequest) {
	rep := proto.PeerReply{RID: req.RID}
	switch req.Op {
	case "list":
		rep.Sessions = s.Peers(from.owner, from.id)
	case "msg":
		d, err := s.PeerSend(from.owner, from.id, req.To, req.Text, req.Wake)
		if err != nil {
			rep.Error = err.Error()
		}
		rep.ID, rep.Name, rep.Busy, rep.Started = d.ID, d.Name, d.Busy, d.Started
	default:
		rep.Error = fmt.Sprintf("unknown peer op %q", req.Op)
	}
	// An error is the requester gone; its tool call ends on its own timeout.
	_ = from.sendLoaded(proto.NewCommand("", &rep))
}

// Peers is principal's live sessions, as dun_list shows them, in id order;
// self is the asking session, marked You.
func (s *Service) Peers(principal, self string) []proto.PeerSession {
	var out []proto.PeerSession
	for _, ses := range s.all() {
		if !ses.isMember(principal) || !ses.loaded() {
			continue
		}
		out = append(out, ses.peerRow(self))
	}
	nameSessions(out)
	return out
}

// PeerDelivery is what PeerSend did.
type PeerDelivery struct {
	ID      string // the target's id
	Name    string // and its name
	Busy    bool   // it was in a turn: the message rides that turn
	Started bool   // it was idle and woken: a turn starts there
}

// PeerSend delivers text from session from to session to (a name from
// dun_list, or an id), both principal's.
func (s *Service) PeerSend(principal, from, to, text string, wake bool) (PeerDelivery, error) {
	to = strings.TrimSpace(to)
	switch {
	case to == "":
		return PeerDelivery{}, errors.New("dun_msg needs `to`: a session name or id from dun_list")
	case strings.TrimSpace(text) == "":
		return PeerDelivery{}, errors.New("dun_msg needs `text`")
	}
	if _, err := s.Get(principal, from); err != nil {
		return PeerDelivery{}, err
	}
	live := s.Peers(principal, from)
	var src, dst *proto.PeerSession
	var named, others []*proto.PeerSession
	for i := range live {
		p := &live[i]
		if p.ID == from {
			src = p
		} else {
			others = append(others, p)
		}
		if p.ID == to {
			dst = p
		}
		if p.Name == to {
			named = append(named, p)
		}
	}
	if dst == nil && len(named) == 1 {
		dst = named[0]
	}
	if dst == nil {
		if len(named) > 1 {
			return PeerDelivery{}, fmt.Errorf("%q names %d sessions — use one of: %s", to, len(named), candidates(named))
		}
		// Registered but not running: say so, rather than "unknown". Someone
		// else's session is "no such session" (see Get).
		if ses, err := s.Get(principal, to); err == nil && !ses.loaded() {
			return PeerDelivery{}, fmt.Errorf("session %s is not loaded in a running engine — only live sessions take messages", to)
		}
		if len(others) == 0 {
			return PeerDelivery{}, fmt.Errorf("no live session %q: there is no other live session", to)
		}
		return PeerDelivery{}, fmt.Errorf("no live session %q — the live ones: %s", to, candidates(others))
	}
	if dst.ID == from {
		return PeerDelivery{}, errors.New("that is this session: dun_msg is for another session")
	}
	if src == nil {
		return PeerDelivery{}, errors.New("this session is not loaded in a running engine")
	}
	ses, err := s.Get(principal, dst.ID)
	if err != nil {
		return PeerDelivery{}, err
	}
	if !s.peerAllow(from, dst.ID, time.Now()) {
		return PeerDelivery{}, fmt.Errorf("rate limit: at most %d messages a minute to %s — wait for its reply", peerRateMax, dst.Name)
	}
	d := PeerDelivery{ID: dst.ID, Name: dst.Name, Busy: ses.busy()}
	d.Started = wake && !d.Busy
	msg := proto.PeerMsg{From: from, FromName: src.Name, Text: text, Wake: wake}
	if err := ses.sendLoaded(proto.NewCommand("", &msg)); err != nil {
		return PeerDelivery{}, fmt.Errorf("%s: %w", dst.Name, err)
	}
	return d, nil
}

func candidates(ps []*proto.PeerSession) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name+" ("+p.ID+")")
	}
	return strings.Join(out, ", ")
}

// nameSessions names each row "<folder>:<hhmmss>" (USER 2026-10-23). folder
// is the (source) workspace's base name, never a worktree's; workspaces that share one but differ get
// parent folders until they do not (src/dun vs old/dun), and the others keep
// the plain base name. hhmmss is the id past its date ("20261021-222836" →
// "222836"). A name still shared (one workspace, one time of day, two dates)
// falls back to the id, for each session that shares it.
func nameSessions(rows []proto.PeerSession) {
	byBase := map[string][]string{} // base name → its distinct workspaces
	for _, r := range rows {
		ws := filepath.Clean(r.Workspace)
		b := filepath.Base(ws)
		if !slices.Contains(byBase[b], ws) {
			byBase[b] = append(byBase[b], ws)
		}
	}
	folder := map[string]string{} // workspace → its folder part
	for _, wss := range byBase {
		// Each workspace takes the fewest components that tell it from every
		// other in its group: src/dun beside old/dun, x/src/dun beside
		// b/src/dun.
		for _, ws := range wss {
			n := 1
			for ; n < 64; n++ {
				unique := true
				for _, o := range wss {
					if o != ws && lastParts(o, n) == lastParts(ws, n) {
						unique = false
						break
					}
				}
				if unique {
					break
				}
			}
			folder[ws] = lastParts(ws, n)
		}
	}
	count := map[string]int{}
	for i := range rows {
		rows[i].Name = folder[filepath.Clean(rows[i].Workspace)] + ":" + timeOfID(rows[i].ID)
		count[rows[i].Name]++
	}
	for i := range rows {
		if count[rows[i].Name] > 1 {
			rows[i].Name = rows[i].ID
		}
	}
}

// lastParts is the last n components of path p.
func lastParts(p string, n int) string {
	parts := strings.Split(strings.Trim(filepath.ToSlash(p), "/"), "/")
	if n > len(parts) {
		n = len(parts)
	}
	return strings.Join(parts[len(parts)-n:], "/")
}

// timeOfID is a session id past its date part: "20261021-222836" →
// "222836", "20261021-222836-2" → "222836-2". An id of another shape is
// itself.
func timeOfID(id string) string {
	if _, rest, ok := strings.Cut(id, "-"); ok && rest != "" {
		return rest
	}
	return id
}

// peerAllow counts a message from → to at now, reporting whether it is
// within the rate.
func (s *Service) peerAllow(from, to string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peerSent == nil {
		s.peerSent = map[[2]string][]time.Time{}
	}
	key := [2]string{from, to}
	var kept []time.Time
	for _, t := range s.peerSent[key] {
		if now.Sub(t) < peerRateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= peerRateMax {
		s.peerSent[key] = kept
		return false
	}
	s.peerSent[key] = append(kept, now)
	return true
}

// loaded: an engine runs this session now.
func (s *session) loaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running && s.eng != nil
}

// busy: in a turn, or waiting on a question inside one.
func (s *session) busy() bool {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.inTurn || s.asking
}

// peerRow is the session's row, unnamed (nameSessions names the list).
func (s *session) peerRow(self string) proto.PeerSession {
	s.actMu.Lock()
	wt, branch, last := s.worktree, s.branch, s.lastUser
	s.actMu.Unlock()
	if branch == "" {
		branch = gitBranch(s.workspace)
	}
	// Workspace is what the session was created in — the source checkout,
	// also when the engine works in a dun worktree — so the name never
	// becomes a dun-worktree-XXXX directory's.
	row := proto.PeerSession{ID: s.id, Workspace: s.workspace, Worktree: wt, Branch: branch, State: "idle",
		Attached: s.log.Subscribers(), You: s.id == self}
	if s.busy() {
		row.State = "busy"
	}
	task := sessionTask(s.workspace, s.id)
	if task == "" {
		task = last
	}
	row.Task = clipLine(task, peerTaskMax)
	return row
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// gitBranch is the branch checked out in dir, read from its HEAD (a worktree's
// .git is a file naming the real git dir); "" when there is none or HEAD is
// detached.
func gitBranch(dir string) string {
	gitDir := filepath.Join(dir, ".git")
	if b, err := os.ReadFile(gitDir); err == nil {
		p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if !ok {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		gitDir = p
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return ref
}

// sendLoaded writes a command to the engine that runs now; it never starts
// one.
func (s *session) sendLoaded(c proto.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil || !s.running {
		return errors.New("engine not running")
	}
	return s.write(c)
}

// announcePeers tells each of owner's loaded sessions how many others are
// loaded (proto.PeerPresence): an engine lists dun_list and dun_msg only once
// there was another. Called when an engine starts or exits.
func (s *Service) announcePeers(owner string) {
	var live []*session
	for _, ses := range s.all() {
		if ses.isMember(owner) && ses.loaded() {
			live = append(live, ses)
		}
	}
	for _, ses := range live {
		_ = ses.sendLoaded(proto.NewCommand("", &proto.PeerPresence{Others: len(live) - 1}))
	}
}
