package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/proto"
)

// `dun msg`: dun↔dun messaging from a shell (USER 2026-11-04). Sessions
// talk with dun_list and dun_msg; this lets a script, a person or another
// agent join in, through dund (proto/msg.go, internal/dund/mailbox.go).

const msgHelp = `dun msg — message dun sessions, and listen for messages, from a shell

  dun msg --who                       who a message can go to: your live
                                      sessions and the listeners
  dun msg --name <name>               listen as <name>: prints token=<token>,
                                      then each message sent to <name>, until
                                      it is stopped; the name and the token end
                                      with it. A name in use is refused.
  dun msg [--as-token=<token>] [--no-wake] <to> <message...>
                                      send to a session (its name from --who,
                                      or its id) or a listener; with a token,
                                      as that listener's name, else as "cli".
                                      <message> "-" reads it from stdin. An
                                      idle session is woken unless --no-wake.

What a listener prints, one message at a time:

  FROM>MESSAGE                        a message of one line
  FROM><h>                            a message of several lines: its lines,
  ...                                 then </h> alone on a line; h is a hash
  </h>                                of the message, so no line of it is </h>

Sessions see listeners in dun_list and reach them with dun_msg; a reply
from a session comes FROM its name. dund keeps listeners in memory: when
dund restarts, a listener stops with an error and must listen again.
Messages wait (up to 200) while a listener is slow to read.

  -h, --help                          this text
`

func msgCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var who, noWake bool
	var name, token string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, bool) {
			if _, v, ok := strings.Cut(a, "="); ok {
				return v, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, msgHelp)
			return 0
		case a == "--who":
			who = true
		case a == "--no-wake":
			noWake = true
		case a == "--name" || strings.HasPrefix(a, "--name="):
			v, ok := val()
			if !ok {
				fmt.Fprintln(stderr, "dun msg: --name needs a name")
				return 2
			}
			name = v
		case a == "--as-token" || strings.HasPrefix(a, "--as-token="):
			v, ok := val()
			if !ok {
				fmt.Fprintln(stderr, "dun msg: --as-token needs a token")
				return 2
			}
			token = v
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(stderr, "dun msg: no flag %s\n\n%s", a, msgHelp)
			return 2
		default:
			rest = append(rest, a)
		}
	}
	var err error
	switch {
	case who:
		err = msgWho(stdout)
	case name != "":
		err = msgListen(name, stdout)
	case len(rest) >= 2:
		text := strings.Join(rest[1:], " ")
		if text == "-" {
			b, rerr := io.ReadAll(stdin)
			if rerr != nil {
				err = rerr
				break
			}
			text = strings.TrimRight(string(b), "\n")
		}
		err = msgSend(proto.MsgSend{Token: token, To: rest[0], Text: text, NoWake: noWake}, stdout)
	default:
		fmt.Fprint(stderr, msgHelp)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "dun msg:", err)
		return 1
	}
	return 0
}

// msgConn is a connection to dund for one dun msg.
type msgConn struct {
	enc *json.Encoder
	dec *json.Decoder
	c   io.Closer
}

func dialMsg() (*msgConn, error) {
	c, err := tui.DialDund()
	if err != nil {
		return nil, fmt.Errorf("dund: %w", err)
	}
	return &msgConn{enc: json.NewEncoder(c), dec: json.NewDecoder(c), c: c}, nil
}

// ask sends cmd and returns dund's answer to it; an ack's error is the error.
func (m *msgConn) ask(cmd proto.Command) (proto.Event, error) {
	if err := m.enc.Encode(cmd); err != nil {
		return proto.Event{}, err
	}
	for {
		var ev proto.Event
		if err := m.dec.Decode(&ev); err != nil {
			return ev, err
		}
		if ev.Re != cmd.CID {
			continue
		}
		if a, ok := ev.Payload.(*proto.Ack); ok && a.Error != "" {
			return ev, errors.New(a.Error)
		}
		return ev, nil
	}
}

func msgWho(w io.Writer) error {
	m, err := dialMsg()
	if err != nil {
		return err
	}
	defer m.c.Close()
	ev, err := m.ask(proto.NewCommand("who", &proto.MsgWho{}))
	if err != nil {
		return err
	}
	l, ok := ev.Payload.(*proto.MsgWhoList)
	if !ok {
		return fmt.Errorf("dund answered %s: is it older than this dun? (dun dund … restart it)", ev.Type)
	}
	if len(l.Sessions) == 0 && len(l.Listeners) == 0 {
		fmt.Fprintln(w, "no live session and no listener")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range l.Sessions {
		fmt.Fprintf(tw, "%s\tsession\t%s\t%s\n", s.Name, s.State, s.Task)
	}
	for _, n := range l.Listeners {
		fmt.Fprintf(tw, "%s\tlistener\t\t\n", n)
	}
	return tw.Flush()
}

func msgSend(p proto.MsgSend, w io.Writer) error {
	m, err := dialMsg()
	if err != nil {
		return err
	}
	defer m.c.Close()
	ev, err := m.ask(proto.NewCommand("send", &p))
	if err != nil {
		return err
	}
	if s, ok := ev.Payload.(*proto.MsgSent); ok {
		fmt.Fprintf(w, "sent to %s (%s)\n", s.To, s.Detail)
		return nil
	}
	return fmt.Errorf("dund answered %s: is it older than this dun?", ev.Type)
}

func msgListen(name string, w io.Writer) error {
	m, err := dialMsg()
	if err != nil {
		return err
	}
	defer m.c.Close()
	ev, err := m.ask(proto.NewCommand("listen", &proto.MsgListen{Name: name}))
	if err != nil {
		return err
	}
	t, ok := ev.Payload.(*proto.MsgToken)
	if !ok {
		return fmt.Errorf("dund answered %s: is it older than this dun?", ev.Type)
	}
	fmt.Fprintf(w, "token=%s\n", t.Token)
	for {
		var ev proto.Event
		if err := m.dec.Decode(&ev); err != nil {
			return fmt.Errorf("dund went away (%v): listen again for a new token", err)
		}
		if in, ok := ev.Payload.(*proto.MsgIn); ok {
			fmt.Fprint(w, msgLine(in.From, in.Text))
		}
	}
}

// msgLine is one message as a listener prints it: FROM>MESSAGE, or, for a
// message of several lines, FROM><h> … </h>, h a hash no line of it equals.
func msgLine(from, text string) string {
	if !strings.Contains(text, "\n") {
		return from + ">" + text + "\n"
	}
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])
	n := 8
	for n < len(h) && strings.Contains(text, "</"+h[:n]+">") {
		n++
	}
	tag := h[:n]
	return from + "><" + tag + ">\n" + strings.TrimRight(text, "\n") + "\n</" + tag + ">\n"
}
