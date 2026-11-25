package acp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// Session config options (docs/acp.md, "Config options"): the model and the
// thinking level. The engine owns both: the bridge keeps the last event of
// each kind it lists them from (settings, the effort command, models) and
// sends the same controls the other clients send. A change from any client
// is a config_option_update.

const (
	optModel  = "model"
	optEffort = "effort"
)

// configWait bounds session/new's wait for the model list and the
// session's settings: the response carries the options when they come in
// time, a config_option_update when they come later. A variable for tests.
var configWait = 5 * time.Second

// setWait bounds set_config_option's wait for the engine to report the new
// value; a model switch starts a new engine.
const (
	setWait    = 15 * time.Second
	switchWait = 2 * time.Minute
)

// config is what the options are made from: the latest of each event.
type config struct {
	settings  *proto.Settings
	effort    *proto.CommandSpec
	providers []proto.ModelProvider
	models    bool // a models event came
}

// modelRef is what a model value switches to (dund's switch_model).
type modelRef struct{ url, model string }

// modelValue is a model's option value: provider and id, as /model names a
// role's model. A value is looked up, never parsed (ids hold "/" too).
func modelValue(provider, model string) string { return provider + "/" + model }

// modelOption is the model selector: every saved provider's models (the
// models event), grouped by provider when there are several, and the
// session's model, listed even when its provider's list did not come.
func (c config) modelOption() (*sdk.SessionConfigOption, map[string]modelRef) {
	st := c.settings
	if st == nil || st.Model == "" {
		return nil, nil
	}
	refs := map[string]modelRef{}
	type group struct {
		name string
		opts []sdk.SessionConfigSelectOption
	}
	var groups []*group
	current := ""
	for _, p := range c.providers {
		if p.Error != "" || len(p.IDs) == 0 {
			continue
		}
		g := &group{name: p.Name}
		for _, id := range p.IDs {
			v := modelValue(p.Name, id)
			if _, dup := refs[v]; dup {
				continue
			}
			refs[v] = modelRef{url: p.URL, model: id}
			g.opts = append(g.opts, sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(v), Name: id})
			if p.URL == st.URL && id == st.Model {
				current = v
			}
		}
		groups = append(groups, g)
	}
	if current == "" {
		name := st.Provider
		for _, p := range c.providers {
			if p.URL == st.URL {
				name = p.Name
			}
		}
		if name == "" {
			name = st.URL
		}
		current = modelValue(name, st.Model)
		refs[current] = modelRef{url: st.URL, model: st.Model}
		opt := sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(current), Name: st.Model}
		i := slices.IndexFunc(groups, func(g *group) bool { return g.name == name })
		if i < 0 {
			groups = append(groups, &group{name: name})
			i = len(groups) - 1
		}
		groups[i].opts = append([]sdk.SessionConfigSelectOption{opt}, groups[i].opts...)
	}
	var opts sdk.SessionConfigSelectOptions
	if len(groups) == 1 {
		flat := sdk.SessionConfigSelectOptionsUngrouped(groups[0].opts)
		opts.Ungrouped = &flat
	} else {
		var gs sdk.SessionConfigSelectOptionsGrouped
		for _, g := range groups {
			gs = append(gs, sdk.SessionConfigSelectGroup{Group: sdk.SessionConfigGroupId(g.name), Name: g.name, Options: g.opts})
		}
		opts.Grouped = &gs
	}
	return &sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: optModel, Name: "Model", Category: sdk.Ptr(sdk.SessionConfigOptionCategoryModel),
		Description:  sdk.Ptr("The session's model; a switch restarts its engine on the same conversation (dun's /model, this session only)."),
		CurrentValue: sdk.SessionConfigValueId(current), Options: opts,
	}}, refs
}

// effortOption is the thinking level: the effort command's levels and the
// one in force (its state; the settings' level before a commands event).
func (c config) effortOption() (*sdk.SessionConfigOption, []string) {
	if c.effort == nil || len(c.effort.Choices) == 0 {
		return nil, nil
	}
	current := c.effort.State
	if current == "" && c.settings != nil {
		current = c.settings.Effort
	}
	var values []string
	var opts sdk.SessionConfigSelectOptionsUngrouped
	for _, ch := range c.effort.Choices {
		values = append(values, ch.Value)
		o := sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(ch.Value), Name: ch.Value}
		if ch.Description != "" {
			o.Description = sdk.Ptr(ch.Description)
		}
		opts = append(opts, o)
	}
	if current == "" {
		return nil, nil
	}
	if !slices.Contains(values, current) {
		values = append(values, current)
		opts = append(opts, sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(current), Name: current})
	}
	return &sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: optEffort, Name: "Thinking", Category: sdk.Ptr(sdk.SessionConfigOptionCategoryThoughtLevel),
		Description:  sdk.Ptr("How much the model thinks before it answers (dun's /effort; saved for every session)."),
		CurrentValue: sdk.SessionConfigValueId(current), Options: sdk.SessionConfigSelectOptions{Ungrouped: &opts},
	}}, values
}

// options is the full list, in the order an editor shows them.
func (c config) options() []sdk.SessionConfigOption {
	out := []sdk.SessionConfigOption{}
	if o, _ := c.modelOption(); o != nil {
		out = append(out, *o)
	}
	if o, _ := c.effortOption(); o != nil {
		out = append(out, *o)
	}
	return out
}

func optionsKey(opts []sdk.SessionConfigOption) string {
	b, _ := json.Marshal(opts)
	return string(b)
}

// onConfig applies an event to the config; once the editor has the
// options (live), a change is a config_option_update, unless a
// set_config_option is under way, whose response carries it.
func (s *session) onConfig(apply func(c *config)) {
	s.mu.Lock()
	apply(&s.cfg)
	close(s.cfgWake)
	s.cfgWake = make(chan struct{})
	opts := s.cfg.options()
	key := optionsKey(opts)
	send := s.cfgLive && s.setting == 0 && key != s.cfgSent
	if send {
		s.cfgSent = key
	}
	s.mu.Unlock()
	if send {
		s.update(sdk.SessionUpdate{ConfigOptionUpdate: &sdk.SessionConfigOptionUpdate{ConfigOptions: opts}})
	}
}

// effortSpec is the effort command in a commands event, nil without one.
func effortSpec(c *proto.Commands) *proto.CommandSpec {
	for i := range c.Items {
		if c.Items[i].Name == "effort" {
			spec := c.Items[i]
			return &spec
		}
	}
	return nil
}

// awaitConfig waits, up to configWait, for the model list and the
// session's settings and thinking levels.
func (s *session) awaitConfig(ctx context.Context) {
	deadline := time.After(configWait)
	for {
		s.mu.Lock()
		have := s.cfg.models && s.cfg.settings != nil && s.cfg.effort != nil
		wake := s.cfgWake
		s.mu.Unlock()
		if have {
			return
		}
		select {
		case <-wake:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// liveConfig is the options the editor gets in a session/new or load
// response; from now on a change is a config_option_update.
func (s *session) liveConfig() []sdk.SessionConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := s.cfg.options()
	s.cfgSent, s.cfgLive = optionsKey(opts), true
	return opts
}

func invalidValue(id, value, why string) error {
	return sdk.NewInvalidParams(map[string]any{"error": why, "configId": id, "value": value})
}

// setConfig is set_config_option: the control another client would send,
// then a wait for the engine to report the value. It answers with the full
// list as it is then.
func (s *session) setConfig(ctx context.Context, id, value string) ([]sdk.SessionConfigOption, error) {
	s.mu.Lock()
	cfg := s.cfg
	waiting := len(s.prompts) > 0
	s.mu.Unlock()
	var cmd any
	var done func(c config) bool
	wait := setWait
	switch id {
	case optModel:
		_, refs := cfg.modelOption()
		ref, ok := refs[value]
		if !ok {
			return nil, invalidValue(id, value, "not one of the model option's values")
		}
		done = func(c config) bool {
			return c.settings != nil && c.settings.URL == ref.url && c.settings.Model == ref.model
		}
		if done(cfg) {
			return s.liveConfig(), nil
		}
		// The switch restarts the engine: a turn of the editor's would end
		// with no done, and its prompt would never be answered.
		if waiting {
			return nil, sdk.NewInvalidRequest(map[string]any{"error": "a prompt of this session is running: switch the model after it ends, or cancel it"})
		}
		cmd = &proto.SwitchModel{URL: ref.url, Model: ref.model}
		wait = switchWait
	case optEffort:
		_, values := cfg.effortOption()
		if !slices.Contains(values, value) {
			return nil, invalidValue(id, value, "not one of the thinking option's values")
		}
		done = func(c config) bool { return c.effort != nil && c.effort.State == value }
		if done(cfg) {
			return s.liveConfig(), nil
		}
		cmd = &proto.ControlCmd{ID: "effort", Action: value}
	default:
		return nil, sdk.NewInvalidParams(map[string]any{"error": "no such config option", "configId": id})
	}

	cid := s.link0().nextCID("cfg")
	acked := make(chan string, 1)
	s.mu.Lock()
	s.setting++
	s.cfgAcks[cid] = acked
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.setting--
		delete(s.cfgAcks, cid)
		s.mu.Unlock()
	}()
	if err := s.send(cid, cmd); err != nil {
		return nil, err
	}
	deadline := time.After(wait)
	for {
		s.mu.Lock()
		ok := done(s.cfg)
		wake := s.cfgWake
		s.mu.Unlock()
		if ok {
			return s.liveConfig(), nil
		}
		select {
		case <-wake:
		case msg := <-acked:
			return nil, sdk.NewInternalError(map[string]any{"error": msg, "configId": id})
		case <-deadline:
			return nil, sdk.NewInternalError(map[string]any{"error": "dun did not report the new value in " + wait.String(), "configId": id})
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, errors.New("the editor's connection ended")
		}
	}
}

// cfgAck hands a set_config_option's error ack to it; false when re is not
// one.
func (s *session) cfgAck(re, msg string) bool {
	s.mu.Lock()
	ch := s.cfgAcks[re]
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- msg:
	default:
	}
	return true
}
