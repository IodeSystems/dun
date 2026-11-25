package acp

import (
	"context"
	"errors"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

func effortCommands(state string) *proto.Commands {
	return &proto.Commands{Items: []proto.CommandSpec{{Name: "effort", State: state, Choices: []proto.CommandChoice{
		{Value: "default", Description: "the server decides"}, {Value: "none", Description: "no thinking"}, {Value: "high"}}}}}
}

// configRig is a session whose engine reports a model on provider a, two
// providers' models and the effort levels.
func configRig(t *testing.T) (*rig, sdk.NewSessionResponse) {
	t.Helper()
	r := newRig(t)
	r.d.state = []any{&proto.Settings{Model: "m1", URL: "http://a", Provider: "a", Effort: "high"}, effortCommands("high")}
	r.d.models = &proto.Models{Providers: []proto.ModelProvider{
		{Name: "a", URL: "http://a", IDs: []string{"m1", "org/m2"}},
		{Name: "b", URL: "http://b", IDs: []string{"x"}},
		{Name: "down", URL: "http://c", Error: "unreachable"}}}
	res, err := r.cc.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: r.cwd, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	return r, res
}

// current is option id's current value in opts, "" when it is absent.
func current(opts []sdk.SessionConfigOption, id string) string {
	for _, o := range opts {
		if o.Select != nil && string(o.Select.Id) == id {
			return string(o.Select.CurrentValue)
		}
	}
	return ""
}

func configUpdates(r *rig) [][]sdk.SessionConfigOption {
	var out [][]sdk.SessionConfigOption
	for _, u := range r.ed.take() {
		if u.ConfigOptionUpdate != nil {
			out = append(out, u.ConfigOptionUpdate.ConfigOptions)
		}
	}
	return out
}

func set(r *rig, sid sdk.SessionId, id, value string) (sdk.SetSessionConfigOptionResponse, error) {
	return r.cc.SetSessionConfigOption(context.Background(), sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
		SessionId: sid, ConfigId: sdk.SessionConfigId(id), Value: sdk.SessionConfigValueId(value)}})
}

// session/new lists the model (grouped by provider, a provider that failed
// left out) and the thinking level, each with the engine's value.
func TestConfigOptionsOnNew(t *testing.T) {
	_, res := configRig(t)
	opts := res.ConfigOptions
	if len(opts) != 4 || opts[0].Select == nil || opts[1].Select == nil {
		t.Fatalf("options %+v", opts)
	}
	m, e := opts[0].Select, opts[1].Select
	if m.Id != optModel || *m.Category != sdk.SessionConfigOptionCategoryModel || m.CurrentValue != "a/m1" {
		t.Errorf("model %+v", m)
	}
	g := m.Options.Grouped
	if g == nil || len(*g) != 2 || (*g)[0].Name != "a" || len((*g)[0].Options) != 2 || (*g)[0].Options[1].Value != "a/org/m2" || (*g)[1].Options[0].Value != "b/x" {
		t.Errorf("model groups %+v", m.Options)
	}
	if e.Id != optEffort || *e.Category != sdk.SessionConfigOptionCategoryThoughtLevel || e.CurrentValue != "high" ||
		e.Options.Ungrouped == nil || len(*e.Options.Ungrouped) != 3 || *(*e.Options.Ungrouped)[0].Description != "the server decides" {
		t.Errorf("effort %+v", e)
	}
}

// Setting the thinking level sends /effort's control and answers with the
// list once the engine reports it; no update repeats the answer.
func TestSetEffort(t *testing.T) {
	r, res := configRig(t)
	r.ed.take()
	ch := make(chan sdk.SetSessionConfigOptionResponse, 1)
	go func() {
		out, err := set(r, res.SessionId, optEffort, "none")
		if err != nil {
			t.Error(err)
		}
		ch <- out
	}()
	c := r.d.next()
	if cc, ok := c.Payload.(*proto.ControlCmd); !ok || cc.ID != "effort" || cc.Action != "none" {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.ControlEvent{ID: "effort", Message: "effort: none"}, effortCommands("none"))
	select {
	case out := <-ch:
		if current(out.ConfigOptions, optEffort) != "none" || current(out.ConfigOptions, optModel) != "a/m1" {
			t.Fatalf("answer %+v", out.ConfigOptions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("set_config_option not answered")
	}
	if u := configUpdates(r); len(u) != 0 {
		t.Errorf("an update repeats the answer: %+v", u)
	}
}

// Setting the model sends switch_model with the provider's URL; the new
// engine's settings answer it.
func TestSetModel(t *testing.T) {
	r, res := configRig(t)
	ch := make(chan sdk.SetSessionConfigOptionResponse, 1)
	go func() {
		out, err := set(r, res.SessionId, optModel, "b/x")
		if err != nil {
			t.Error(err)
		}
		ch <- out
	}()
	c := r.d.next()
	if sm, ok := c.Payload.(*proto.SwitchModel); !ok || sm.URL != "http://b" || sm.Model != "x" || c.Session != "s1" {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.Settings{Model: "x", URL: "http://b", Provider: "b", Effort: "high"})
	select {
	case out := <-ch:
		if current(out.ConfigOptions, optModel) != "b/x" {
			t.Fatalf("answer %+v", out.ConfigOptions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("set_config_option not answered")
	}
}

// A change another client makes (the TUI's /effort, /model) is a
// config_option_update with the full list; an event that changes nothing
// sends none.
func TestConfigUpdateFromElsewhere(t *testing.T) {
	r, _ := configRig(t)
	r.ed.take()
	r.d.emit(effortCommands("high")) // as it was
	r.d.emit(effortCommands("default"))
	r.d.emit(&proto.Settings{Model: "org/m2", URL: "http://a", Provider: "a", Effort: "default"})
	deadline := time.Now().Add(5 * time.Second)
	var got [][]sdk.SessionConfigOption
	for len(got) < 2 && time.Now().Before(deadline) {
		got = append(got, configUpdates(r)...)
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 2 || current(got[0], optEffort) != "default" || current(got[1], optModel) != "a/org/m2" || len(got[1]) != 4 {
		t.Fatalf("updates %+v", got)
	}
}

// A value not listed, an unknown option and a model switch while a prompt
// waits are refused with a JSON-RPC error; nothing is sent.
func TestSetConfigRefused(t *testing.T) {
	r, res := configRig(t)
	for _, c := range []struct{ id, value string }{{optEffort, "bogus"}, {optModel, "a/nope"}, {"speed", "fast"}} {
		_, err := set(r, res.SessionId, c.id, c.value)
		var re *sdk.RequestError
		if !errors.As(err, &re) || re.Code != -32602 {
			t.Errorf("%s=%s: want -32602, got %v", c.id, c.value, err)
		}
	}
	ch := r.prompt(res.SessionId, "work")
	r.d.next()
	if _, err := set(r, res.SessionId, optModel, "b/x"); err == nil {
		t.Error("a model switch while a prompt waits")
	}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "work"}, &proto.Turn{Kind: "user"}, &proto.Done{})
	waitPrompt(t, ch)
	select {
	case c := <-r.d.cmds:
		if c.Type != "detach" {
			t.Errorf("sent %s %+v", c.Type, c.Payload)
		}
	default:
	}
}

// The value already in force answers at once, sending nothing.
func TestSetConfigSameValue(t *testing.T) {
	r, res := configRig(t)
	out, err := set(r, res.SessionId, optModel, "a/m1")
	if err != nil || current(out.ConfigOptions, optModel) != "a/m1" {
		t.Fatalf("%+v %v", out, err)
	}
	select {
	case c := <-r.d.cmds:
		t.Errorf("sent %s", c.Type)
	case <-time.After(100 * time.Millisecond):
	}
}

// A dund that refuses the switch fails set_config_option with its reason.
func TestSetModelRefusedByDund(t *testing.T) {
	r, res := configRig(t)
	ch := make(chan error, 1)
	go func() {
		_, err := set(r, res.SessionId, optModel, "b/x")
		ch <- err
	}()
	c := r.d.next()
	ack := proto.NewEvent(&proto.Ack{Error: "no such session"})
	ack.Re, ack.Session = c.CID, "s1"
	r.d.mu.Lock()
	for _, conn := range r.d.subs {
		r.d.write(conn, ack)
	}
	r.d.mu.Unlock()
	select {
	case err := <-ch:
		var re *sdk.RequestError
		if !errors.As(err, &re) || re.Code != -32603 {
			t.Fatalf("want -32603, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not answered")
	}
}
