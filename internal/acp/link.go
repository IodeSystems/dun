package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// link is one connection to dund, for one session: commands out, the
// session's events in. As the TUI's (internal/tui/attach.go), it is dund's
// client and nothing more.
type link struct {
	c  net.Conn
	sc *bufio.Scanner
	mu sync.Mutex // the writes
	n  int
	// early are the events read while call waited for its answer: the start
	// of the stream, for the reader.
	early []proto.Event
}

// callWait bounds a call: a create waits for a new engine to name its
// session (the TUI waits as long).
const callWait = 4 * time.Minute

func newLink(c net.Conn) *link {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	return &link{c: c, sc: sc}
}

// send writes a command for session; cid "" asks for no answer.
func (l *link) send(session, cid string, payload any) error {
	cmd := proto.NewCommand(cid, payload)
	cmd.Session = session
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.c.Write(append(b, '\n'))
	return err
}

// nextCID is a fresh command id.
func (l *link) nextCID(prefix string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return fmt.Sprintf("%s%d", prefix, l.n)
}

// call sends a command and reads until the event that answers it, keeping
// the events read on the way. Only before the reader runs.
func (l *link) call(session string, payload any) (proto.Event, error) {
	cid := l.nextCID("acp")
	if err := l.send(session, cid, payload); err != nil {
		return proto.Event{}, err
	}
	_ = l.c.SetReadDeadline(time.Now().Add(callWait))
	defer l.c.SetReadDeadline(time.Time{})
	for l.sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(l.sc.Bytes(), &ev) != nil {
			continue
		}
		ack, isAck := ev.Payload.(*proto.Ack)
		if ev.Re == cid {
			if isAck && ack.Error != "" {
				return ev, errors.New(ack.Error)
			}
			return ev, nil
		}
		// An error that answers no command is dund saying why it hangs up.
		if isAck && ev.Re == "" && ack.Error != "" {
			return ev, errors.New(ack.Error)
		}
		l.early = append(l.early, ev)
	}
	if err := l.sc.Err(); err != nil {
		return proto.Event{}, err
	}
	return proto.Event{}, errors.New("dund closed the connection")
}

// read hands each event to f, the early ones first, until the connection
// ends; it returns why it ended.
func (l *link) read(f func(proto.Event)) error {
	early := l.early
	l.early = nil
	for _, ev := range early {
		f(ev)
	}
	for l.sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(l.sc.Bytes(), &ev) != nil {
			continue
		}
		f(ev)
	}
	if err := l.sc.Err(); err != nil {
		return err
	}
	return errors.New("dund closed the connection")
}

func (l *link) close() error { return l.c.Close() }
