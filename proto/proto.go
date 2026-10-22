// Package proto is the wire protocol between a dun session and its clients.
//
// One protocol for every transport: the engine's stdio, the service's unix
// socket and, later, a websocket all carry the same NDJSON frames. A frame is
// one flat JSON object — the envelope fields beside the payload's own — so a
// line is readable by eye and by a client that knows only a few types.
//
// Two directions:
//   - Event, session → client: {v, session, seq, ts, type, re?, …payload}.
//     seq numbers a session's events from 1 with no gaps; a client that knows
//     the last seq it saw can resume after it (see Log).
//   - Command, client → session: {v, cid, session, type, …payload}. A
//     command that carries a cid is answered by an "ack" event whose re is
//     that cid. (Not "id": server, control and agent commands already use id
//     for their target.)
//
// Payloads are typed per frame type (events.go, commands.go). A type this
// version does not know decodes to a map rather than failing, so an older
// client keeps working beside a newer engine.
package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Version is the protocol version every frame carries in v.
const Version = 1

// Event is one frame from a session to its clients.
type Event struct {
	V       int       `json:"v"`
	Session string    `json:"session,omitempty"`
	Seq     uint64    `json:"seq,omitempty"`
	TS      time.Time `json:"ts"`
	Type    string    `json:"type"`
	// Re is the id of the command this event answers (an ack).
	Re string `json:"re,omitempty"`
	// Payload is the type's struct from events.go (a pointer), or a
	// map[string]any for a type this version does not know.
	Payload any `json:"-"`
}

// Command is one frame from a client to a session.
type Command struct {
	V       int    `json:"v"`
	CID     string `json:"cid,omitempty"`
	Session string `json:"session,omitempty"`
	Type    string `json:"type"`
	Payload any    `json:"-"`
}

// The envelopes' JSON names; no payload may use them.
var (
	eventKeys   = map[string]bool{"v": true, "session": true, "seq": true, "ts": true, "type": true, "re": true}
	commandKeys = map[string]bool{"v": true, "cid": true, "session": true, "type": true}
)

type eventHeader Event
type commandHeader Command

func (e Event) MarshalJSON() ([]byte, error) {
	return merge(eventHeader(e), e.Payload)
}

func (e *Event) UnmarshalJSON(b []byte) error {
	var h eventHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return err
	}
	p, err := decodePayload(b, eventTypes[h.Type], eventKeys)
	if err != nil {
		return fmt.Errorf("event %s: %w", h.Type, err)
	}
	*e = Event(h)
	e.Payload = p
	return nil
}

func (c Command) MarshalJSON() ([]byte, error) {
	return merge(commandHeader(c), c.Payload)
}

func (c *Command) UnmarshalJSON(b []byte) error {
	var h commandHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return err
	}
	p, err := decodePayload(b, commandTypes[commandType(h.Type)], commandKeys)
	if err != nil {
		return fmt.Errorf("command %s: %w", h.Type, err)
	}
	*c = Command(h)
	c.Payload = p
	return nil
}

// merge writes the header's object and the payload's object as one.
func merge(header, payload any) ([]byte, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return hb, nil
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	pb = bytes.TrimSpace(pb)
	if len(pb) < 2 || pb[0] != '{' {
		return nil, fmt.Errorf("payload %T is not a JSON object", payload)
	}
	if string(pb) == "{}" {
		return hb, nil
	}
	out := make([]byte, 0, len(hb)+len(pb))
	out = append(out, hb[:len(hb)-1]...)
	out = append(out, ',')
	return append(out, pb[1:]...), nil
}

// Strict makes decoding reject a field a payload struct does not have. Off in
// use, so a newer peer's extra field is ignored; tests turn it on to prove
// the structs cover what the engine sends.
var Strict = false

func decodePayload(b []byte, t reflect.Type, header map[string]bool) (any, error) {
	if t == nil {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		for k := range header {
			delete(m, k)
		}
		return m, nil
	}
	p := reflect.New(t).Interface()
	if !Strict {
		return p, json.Unmarshal(b, p)
	}
	// The header's fields share the object; drop them so they are not
	// unknown to the payload.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k := range header {
		delete(m, k)
	}
	rest, _ := json.Marshal(m)
	d := json.NewDecoder(bytes.NewReader(rest))
	d.DisallowUnknownFields()
	return p, d.Decode(p)
}

// NewEvent is an unsequenced event of the payload's type; a Log numbers it.
func NewEvent(payload any) Event {
	return Event{V: Version, Type: TypeOf(payload), Payload: payload}
}

// NewCommand is a command of the payload's type.
func NewCommand(cid string, payload any) Command {
	return Command{V: Version, CID: cid, Type: TypeOf(payload), Payload: payload}
}

// TypeOf is the frame type a payload struct is registered under.
func TypeOf(payload any) string {
	t := reflect.TypeOf(payload)
	if t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n, ok := eventTypeOf[t]; ok {
		return n
	}
	if n, ok := commandTypeOf[t]; ok {
		return n
	}
	panic(fmt.Sprintf("proto: %T is not a registered payload", payload))
}

var (
	eventTypes    = map[string]reflect.Type{}
	eventTypeOf   = map[reflect.Type]string{}
	commandTypes  = map[string]reflect.Type{}
	commandTypeOf = map[reflect.Type]string{}
	eventOrder    []string
	commandOrder  []string
)

func event[T any](name string) {
	t := reflect.TypeFor[T]()
	eventTypes[name], eventTypeOf[t] = t, name
	eventOrder = append(eventOrder, name)
}

func command[T any](name string) {
	t := reflect.TypeFor[T]()
	commandTypes[name], commandTypeOf[t] = t, name
	commandOrder = append(commandOrder, name)
}

// commandType folds the aliases the engine accepts onto one type.
func commandType(name string) string {
	if name == "quit" {
		return "stop"
	}
	return name
}
