package dund

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// connBuffer is how many frames may wait for one slow connection before it
// is closed. Each subscription has its own, larger buffer in the Log.
const connBuffer = 1024

// Principal names who is on the other end of a connection.
type Principal func(net.Conn) (string, error)

// UnixPeer is the principal of a unix socket's peer: "unix:<uid>", from the
// kernel's peer credentials, not from anything the client says.
func UnixPeer(c net.Conn) (string, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return "", errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "", err
	}
	var uid uint32
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		uid, cerr = peerUID(int(fd)) // peer_<os>.go
	}); err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	return "unix:" + strconv.FormatUint(uint64(uid), 10), nil
}

// Server speaks the protocol to clients over a listener.
type Server struct {
	svc       *Service
	principal Principal

	// The build this dund runs, and how to move it to another: set by
	// SetBuild; a nil upgrade means a service manager owns the process and
	// it cannot be upgraded from here (noUpgrade says why).
	version   string
	exe       string
	upgrade   func(exe, version string) error
	noUpgrade string

	mu    sync.Mutex
	conns map[*conn]struct{}
}

func NewServer(svc *Service, principal Principal) *Server {
	s := &Server{svc: svc, principal: principal, conns: map[*conn]struct{}{}}
	// The upgrade status goes to every connection, whatever it follows.
	svc.WatchUpgrade(s.broadcastUpgrade)
	return s
}

// SetBuild names this dund's build for Hello, and how to honour Upgrade (nil:
// refuse, a service manager restarts it). upgrade checks exe and makes the
// upgrade pending (Service.RequestUpgrade).
func (s *Server) SetBuild(version, exe string, upgrade func(exe, version string) error) {
	s.version, s.exe, s.upgrade = version, exe, upgrade
}

// SetNoUpgrade says why Upgrade is refused (a nil upgrade in SetBuild): in
// Hello's reply, and in the refusal.
func (s *Server) SetNoUpgrade(why string) { s.noUpgrade = why }

// broadcastUpgrade queues st for every connection. A connection that cannot
// take it now gets it dropped rather than stalling the others: the next
// change, or its reconnect, brings it up to date.
func (s *Server) broadcastUpgrade(st proto.UpgradeStatus) {
	s.mu.Lock()
	to := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		to = append(to, c)
	}
	s.mu.Unlock()
	for _, c := range to {
		ev := proto.NewEvent(&st)
		ev.TS = time.Now()
		select {
		case c.out <- ev:
		case <-c.done:
		default:
			log.Printf("dund: a connection's queue is full; it misses an upgrade status")
		}
	}
}

// Drain waits, up to d, until every connection has written what is queued
// for it: the "restarting" status before dund execs a new build.
func (s *Server) Drain(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		queued := 0
		for c := range s.conns {
			queued += len(c.out)
		}
		s.mu.Unlock()
		if queued == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Serve accepts connections until ln closes.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		who, err := s.principal(c)
		if err != nil {
			// Said, not just done: a silent close reads at the client as "dund
			// closed the connection", which names nothing to fix.
			log.Printf("dund: refused a connection: cannot tell who it is: %v", err)
			ev := proto.NewEvent(&proto.Ack{Error: "dund cannot tell who is connecting: " + err.Error()})
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			_ = json.NewEncoder(c).Encode(ev)
			_ = c.Close()
			continue
		}
		cn := &conn{srv: s, nc: c, who: who, out: make(chan proto.Event, connBuffer),
			subs: map[string]*proto.Subscription{}, done: make(chan struct{})}
		s.mu.Lock()
		s.conns[cn] = struct{}{}
		s.mu.Unlock()
		if st := s.svc.UpgradeStatus(); st != nil {
			ev := proto.NewEvent(st)
			ev.TS = time.Now()
			cn.out <- ev // the queue is new and empty
		}
		go cn.write()
		go cn.read()
	}
}

// conn is one client connection. It may follow several sessions at once;
// every event names its session.
type conn struct {
	srv  *Server
	nc   net.Conn
	who  string
	out  chan proto.Event
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	subs  map[string]*proto.Subscription // by session id
	build string                         // the client's, from its Hello
	// views are the sub-agent views this connection opened and has not
	// left, by session then agent id: a client that goes without "leave"
	// would otherwise keep the engine streaming that child's entries.
	views map[string]map[string]int
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.nc.Close()
		c.mu.Lock()
		for _, sub := range c.subs {
			sub.Close()
		}
		c.subs = map[string]*proto.Subscription{}
		views := c.views
		c.views = nil
		c.mu.Unlock()
		for ses := range views {
			c.leaveViews(ses, views[ses])
		}
		c.srv.mu.Lock()
		delete(c.srv.conns, c)
		c.srv.mu.Unlock()
	})
}

// send queues a frame; a connection that cannot keep up is closed, not
// waited for.
// sendStall is how long a send waits for the writer before the connection is
// given up as dead.
var sendStall = 10 * time.Second

// send queues an event for the writer, waiting while the queue is full: a
// resume replays a backlog in one burst, and dropping the connection when the
// queue filled (as this did) made every resume of a long session fail the
// same way, silently. A writer that makes no progress for sendStall is a dead
// client: that closes the connection, and says so.
func (c *conn) send(ev proto.Event) {
	select {
	case <-c.done:
		return
	case c.out <- ev:
		return
	default:
	}
	t := time.NewTimer(sendStall)
	defer t.Stop()
	select {
	case <-c.done:
	case c.out <- ev:
	case <-t.C:
		log.Printf("dund: closed a connection: its client read nothing for %s", sendStall)
		c.close()
	}
}

func (c *conn) write() {
	enc := json.NewEncoder(c.nc)
	for {
		select {
		case <-c.done:
			return
		case ev := <-c.out:
			if enc.Encode(ev) != nil {
				c.close()
				return
			}
		}
	}
}

func (c *conn) read() {
	defer c.close()
	sc := newScanner(c.nc)
	for sc.Scan() {
		var cmd proto.Command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			c.ack(cmd, "", fmt.Errorf("bad frame: %w", err))
			continue
		}
		c.handle(cmd)
	}
}

func (c *conn) ack(cmd proto.Command, session string, err error) {
	if cmd.CID == "" && err == nil {
		return
	}
	a := &proto.Ack{}
	if err != nil {
		a.Error = err.Error()
	}
	ev := proto.NewEvent(a)
	ev.Re, ev.Session, ev.TS = cmd.CID, session, time.Now()
	c.send(ev)
}

func (c *conn) reply(cmd proto.Command, session string, payload any) {
	ev := proto.NewEvent(payload)
	ev.Re, ev.Session, ev.TS = cmd.CID, session, time.Now()
	c.send(ev)
}

func (c *conn) handle(cmd proto.Command) {
	svc := c.srv.svc
	switch p := cmd.Payload.(type) {
	case *proto.ListSessions:
		c.reply(cmd, "", &proto.SessionList{Items: svc.List(c.who)})
	case *proto.Create:
		// A handshake with a new engine takes seconds; the connection keeps
		// reading meanwhile.
		go func() {
			if p.ReuseIdle && p.Resume == "" {
				id, reused, err := svc.CreateOrReuse(c.who, p.Workspace, p.Args)
				if err != nil || !reused {
					c.ack(cmd, id, err)
					return
				}
				c.reply(cmd, id, &proto.Ack{Reused: true})
				return
			}
			id, err := svc.Create(c.who, p.Workspace, p.Args, p.Resume)
			c.ack(cmd, id, err)
		}()
	case *proto.Subscribe:
		c.subscribe(cmd, p.After, p.Epoch)
	case *proto.Detach:
		c.mu.Lock()
		if sub, ok := c.subs[cmd.Session]; ok {
			sub.Close()
			delete(c.subs, cmd.Session)
		}
		c.mu.Unlock()
		c.ack(cmd, cmd.Session, nil)
	case *proto.UIGet:
		st, err := svc.UI(c.who, cmd.Session)
		if err != nil {
			c.ack(cmd, cmd.Session, err)
			return
		}
		c.reply(cmd, cmd.Session, &proto.UIState{State: st})
	case *proto.UIPut:
		err := svc.PutUI(c.who, cmd.Session, p.State)
		c.ack(cmd, cmd.Session, err)
		if err == nil {
			c.srv.uiChanged(c, cmd.Session, p.State)
		}
	case *proto.Restart:
		go func() { c.ack(cmd, cmd.Session, svc.Restart(c.who, cmd.Session, p.Args)) }()
	case *proto.Forget:
		go func() { c.ack(cmd, cmd.Session, svc.Forget(c.who, cmd.Session)) }()
	case *proto.Stop:
		c.ack(cmd, cmd.Session, svc.Stop(c.who, cmd.Session))
	case *proto.Hello:
		c.mu.Lock()
		c.build = p.Version
		c.mu.Unlock()
		c.reply(cmd, "", &proto.HelloReply{Version: c.srv.version, Exe: c.srv.exe, Upgradable: c.srv.upgrade != nil,
			NoUpgrade: c.srv.noUpgrade, Busy: svc.Busy(), Coordinated: true, Upgrade: svc.UpgradeStatus()})
	case *proto.Upgrade:
		if p.Exe == "" && p.Now {
			c.ack(cmd, "", svc.UpgradeNow())
			return
		}
		if c.srv.upgrade == nil {
			why := "a service manager runs this dund"
			if c.srv.noUpgrade != "" {
				why += " and it cannot upgrade itself: " + c.srv.noUpgrade
			}
			c.ack(cmd, "", errors.New(why+" — restart the service instead (dun dund service restart)"))
			return
		}
		ver := p.Version
		if ver == "" {
			c.mu.Lock()
			ver = c.build
			c.mu.Unlock()
		}
		err := c.srv.upgrade(p.Exe, ver)
		if err == nil && p.Now {
			err = svc.UpgradeNow()
		}
		c.ack(cmd, "", err)
	case *proto.JobUsageReq:
		c.reply(cmd, cmd.Session, svc.JobUsage(c.who, cmd.Session, p.Job))
	case *proto.Persist:
		on, changed, err := svc.SetPersist(c.who, cmd.Session, p.Mode)
		if err != nil {
			c.ack(cmd, cmd.Session, err)
			return
		}
		c.reply(cmd, cmd.Session, &proto.PersistState{On: on, Changed: changed})
	case *proto.OAuthCmd:
		// `dun oauth token`, for mcpshell: it may wait on a login.
		var ses *session
		if cmd.Session != "" {
			var err error
			if ses, err = svc.Get(c.who, cmd.Session); err != nil {
				c.ack(cmd, cmd.Session, err)
				return
			}
		}
		go func() { c.reply(cmd, cmd.Session, svc.OAuth(context.Background(), ses, p.OAuthReq)) }()
	case *proto.Leave:
		// This connection's subscription goes first, so the service counts the
		// clients still attached without it.
		c.mu.Lock()
		if sub, ok := c.subs[cmd.Session]; ok {
			sub.Close()
			delete(c.subs, cmd.Session)
		}
		views := c.views[cmd.Session]
		delete(c.views, cmd.Session)
		c.mu.Unlock()
		c.leaveViews(cmd.Session, views)
		go func() { c.ack(cmd, cmd.Session, svc.Leave(c.who, cmd.Session)) }()
	default:
		// Anything else is for the session's engine.
		if cmd.Session == "" {
			c.ack(cmd, "", fmt.Errorf("%s needs a session", cmd.Type))
			return
		}
		ses, err := svc.Get(c.who, cmd.Session)
		if err == nil {
			err = ses.send(cmd)
		}
		if a, ok := p.(*proto.AgentCmd); ok && err == nil {
			c.countView(cmd.Session, a)
		}
		c.ack(cmd, cmd.Session, err)
	}
}

// countView keeps the connection's open sub-agent views: a view opens one,
// a leave closes one.
func (c *conn) countView(session string, a *proto.AgentCmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch a.Action {
	case "view":
		if c.views == nil {
			c.views = map[string]map[string]int{}
		}
		if c.views[session] == nil {
			c.views[session] = map[string]int{}
		}
		c.views[session][a.ID]++
	case "leave":
		if n := c.views[session][a.ID]; n > 1 {
			c.views[session][a.ID] = n - 1
		} else {
			delete(c.views[session], a.ID)
		}
	}
}

// leaveViews sends the leaves a departed client did not, to a running
// engine only: a stopped one has no watchers left to drop.
func (c *conn) leaveViews(session string, views map[string]int) {
	if len(views) == 0 {
		return
	}
	ses, err := c.srv.svc.Get(c.who, session)
	if err != nil {
		return
	}
	for id, n := range views {
		for range n {
			_ = ses.sendIfRunning(proto.NewCommand("", &proto.AgentCmd{ID: id, Action: "leave"}))
		}
	}
}

// subscribe catches the connection up on a session and follows it live.
// Following starts the engine if it is not running.
func (c *conn) subscribe(cmd proto.Command, after uint64, epoch string) {
	ses, err := c.srv.svc.Get(c.who, cmd.Session)
	if err != nil {
		c.ack(cmd, cmd.Session, err)
		return
	}
	snap, replay, sub, err := ses.subscribe(after, epoch)
	if err != nil {
		c.ack(cmd, cmd.Session, err)
		return
	}
	c.mu.Lock()
	if old, ok := c.subs[cmd.Session]; ok {
		old.Close()
	}
	c.subs[cmd.Session] = sub
	c.mu.Unlock()
	c.ack(cmd, cmd.Session, nil)
	if snap != nil {
		c.send(*snap)
	}
	for _, ev := range replay {
		c.send(ev)
	}
	go func() {
		var last uint64
		for ev := range sub.C() {
			last = ev.Seq
			c.send(ev)
		}
		// Dropped for falling behind: say so, and from where to resume.
		if errors.Is(sub.Err(), proto.ErrSlow) {
			c.ack(cmd, cmd.Session, fmt.Errorf("dropped: fell behind; subscribe after %d", last))
		}
	}()
	if err := ses.ensureRunning(); err != nil {
		c.reply(proto.Command{}, cmd.Session, &proto.EngineState{State: "gave_up", Reason: err.Error()})
	}
}

// uiChanged sends a member's new UI state to that member's other
// connections following the session. Nobody else sees it.
func (s *Server) uiChanged(from *conn, session string, state map[string]any) {
	s.mu.Lock()
	var to []*conn
	for c := range s.conns {
		if c == from || c.who != from.who {
			continue
		}
		c.mu.Lock()
		_, following := c.subs[session]
		c.mu.Unlock()
		if following {
			to = append(to, c)
		}
	}
	s.mu.Unlock()
	for _, c := range to {
		ev := proto.NewEvent(&proto.UIState{State: state})
		ev.Session, ev.TS = session, time.Now()
		c.send(ev)
	}
}
