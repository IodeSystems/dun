package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fill sets every field of v to a non-zero value, so a round trip cannot pass
// by omitting a field.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint64:
		v.SetUint(7)
	case reflect.Float64:
		v.SetFloat(0.5)
	case reflect.Interface:
		v.Set(reflect.ValueOf("v"))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e)
		m.SetMapIndex(reflect.ValueOf("k"), e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[Event]() {
			v.Set(reflect.ValueOf(Event{V: Version, Seq: 3, TS: time.Date(2026, 10, 21, 1, 2, 3, 0, time.UTC),
				Type: "token", Payload: &Token{Text: "t"}}))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i))
		}
	default:
		panic("fill: " + v.Type().String())
	}
}

func filled(t reflect.Type) any {
	p := reflect.New(t)
	fill(p.Elem())
	return p.Interface()
}

func TestEventRoundTrip(t *testing.T) {
	for _, name := range eventOrder {
		t.Run(name, func(t *testing.T) {
			ev := Event{V: Version, Session: "s1", Seq: 42, TS: time.Date(2026, 10, 21, 1, 2, 3, 0, time.UTC),
				Type: name, Re: "c9", Payload: filled(eventTypes[name])}
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			var got Event
			Strict = true
			defer func() { Strict = false }()
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
			if !reflect.DeepEqual(got, ev) {
				t.Fatalf("round trip differs\nsent %#v\ngot  %#v\nwire %s", ev.Payload, got.Payload, b)
			}
		})
	}
}

func TestCommandRoundTrip(t *testing.T) {
	for _, name := range commandOrder {
		t.Run(name, func(t *testing.T) {
			c := Command{V: Version, CID: "c1", Session: "s1", Type: name, Payload: filled(commandTypes[name])}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			var got Command
			Strict = true
			defer func() { Strict = false }()
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Fatalf("round trip differs\nsent %#v\ngot  %#v\nwire %s", c, got, b)
			}
		})
	}
}

// The frame is one flat object: the envelope beside the payload's fields.
func TestFrameIsFlat(t *testing.T) {
	b, _ := json.Marshal(Event{V: 1, Seq: 5, Type: "control", Payload: &ControlEvent{ID: "docker", Message: "on"}})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "seq", "ts", "type", "id", "message"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%s missing from %s", k, b)
		}
	}
	// A payload with no fields adds nothing.
	b, _ = json.Marshal(NewEvent(&Done{}))
	if strings.Contains(string(b), ",}") || !strings.HasSuffix(string(b), `"type":"done"}`) {
		t.Errorf("empty payload frame: %s", b)
	}
}

// No payload may use an envelope name: the flat frame would carry two values
// for one key.
func TestNoPayloadUsesAnEnvelopeKey(t *testing.T) {
	check := func(kind string, types map[string]reflect.Type, header map[string]bool) {
		for name, typ := range types {
			for _, f := range jsonFields(typ) {
				if header[f] {
					t.Errorf("%s %s: field %q is an envelope key", kind, name, f)
				}
			}
		}
	}
	check("event", eventTypes, eventKeys)
	check("command", commandTypes, commandKeys)
}

// jsonFields is a struct's top-level JSON names, embedded structs flattened.
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			out = append(out, jsonFields(ft)...)
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// A type this version does not know decodes to a map, without the envelope.
func TestUnknownTypeDecodesToMap(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"v":2,"seq":9,"ts":"2026-10-21T00:00:00Z","type":"future","x":1}`), &ev); err != nil {
		t.Fatal(err)
	}
	m, ok := ev.Payload.(map[string]any)
	if !ok || len(m) != 1 || m["x"] != float64(1) || ev.Seq != 9 || ev.V != 2 {
		t.Fatalf("got %#v %#v", ev, ev.Payload)
	}
}

// Not strict, an extra field from a newer peer is ignored.
func TestLenientIgnoresExtraFields(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"v":1,"ts":"2026-10-21T00:00:00Z","type":"token","text":"hi","new":true}`), &ev); err != nil {
		t.Fatal(err)
	}
	if p := ev.Payload.(*Token); p.Text != "hi" {
		t.Fatalf("got %#v", p)
	}
	Strict = true
	defer func() { Strict = false }()
	if err := json.Unmarshal([]byte(`{"v":1,"ts":"2026-10-21T00:00:00Z","type":"token","text":"hi","new":true}`), &ev); err == nil {
		t.Fatal("strict decode accepted an unknown field")
	}
}

func TestQuitIsStop(t *testing.T) {
	var c Command
	if err := json.Unmarshal([]byte(`{"type":"quit"}`), &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Payload.(*Stop); !ok || c.Type != "quit" {
		t.Fatalf("got %#v", c)
	}
}

func TestTypeOf(t *testing.T) {
	if TypeOf(&ToolCall{}) != "tool_call" || TypeOf(Subscribe{}) != "subscribe" {
		t.Fatal("TypeOf")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unregistered type did not panic")
		}
	}()
	TypeOf(42)
}
