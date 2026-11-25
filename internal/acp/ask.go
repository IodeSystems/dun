package acp

import (
	"context"
	"strconv"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// dun's questions (docs/acp.md, "Questions"). One with options to pick one
// of — the file-access question (harness/fileaccess.go), or an ask_user
// with one answer — is a session/request_permission: an option per answer,
// the chosen one sent back as the answer. A multiple-choice question, one
// with no options, or one the editor could not ask, is shown as the agent's
// text, and the next prompt is its answer. An editor that advertises
// elicitation.form gets every question as a form instead (elicit.go).

// onAsk asks a question once (the ask event and turn_state both carry it).
func (s *session) onAsk(a *proto.Ask) {
	s.mu.Lock()
	// The same question again (ask and turn_state both carry it) is not
	// raised twice. After the engine's stop suspended the turn (done
	// suspended) the new engine asks it again, with the same ask_id: one
	// still open (a form or request pending, or shown as text) is kept; one
	// the bridge dropped is raised again.
	held := s.perm == a.AskID || (s.ask != nil && s.ask.AskID == a.AskID)
	again := s.suspended
	s.suspended = false
	if a.AskID != "" && a.AskID == s.lastAsk && (held || !again) {
		s.mu.Unlock()
		return
	}
	s.lastAsk, s.askCall = a.AskID, a.CallID
	form := s.caps.form
	s.mu.Unlock()
	if !form && (a.Multi || len(a.Options) == 0) {
		s.askText(a)
		return
	}
	ctx, stop := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.perm, s.permStop = a.AskID, stop
	s.mu.Unlock()
	if form {
		go s.elicit(ctx, stop, a)
		return
	}
	go s.permission(ctx, stop, a)
}

func (s *session) permission(ctx context.Context, stop context.CancelFunc, a *proto.Ask) {
	defer stop()
	resp, err := s.peer.RequestPermission(ctx, permissionRequest(s.id, a))
	s.mu.Lock()
	if s.perm == a.AskID {
		s.perm, s.permStop = "", nil
	}
	s.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return // answered elsewhere, or the turn ended
	case err != nil:
		s.log.Info("acp: request_permission failed; asking as text", "session", s.id, "err", err)
		s.askText(a)
		return
	case resp.Outcome.Selected != nil:
		i, err := strconv.Atoi(string(resp.Outcome.Selected.OptionId))
		if err != nil || i < 0 || i >= len(a.Options) {
			s.askText(a)
			return
		}
		_ = s.send("", &proto.Answer{Value: a.Options[i], AskID: a.AskID})
	}
	// Cancelled: the editor cancels the turn too (session/cancel), and the
	// interrupt ends the question.
}

// permissionRequest is the question as ACP's: the tool call it belongs to
// (the call that asked; an ask_user's own), an option per answer, by index.
func permissionRequest(session string, a *proto.Ask) sdk.RequestPermissionRequest {
	id := a.CallID
	if id == "" {
		id = "ask-" + a.AskID
	}
	r := sdk.RequestPermissionRequest{
		SessionId: sdk.SessionId(session),
		ToolCall: sdk.ToolCallUpdate{
			ToolCallId: sdk.ToolCallId(id),
			Title:      sdk.PatchSet(a.Question),
		},
	}
	for i, o := range a.Options {
		r.Options = append(r.Options, sdk.PermissionOption{OptionId: sdk.PermissionOptionId(strconv.Itoa(i)), Name: o, Kind: optionKind(o)})
	}
	return r
}

// optionKind reads an answer's words for ACP's kind (the editor's button
// style): a no rejects, a yes scoped beyond the session allows always, and
// anything else allows once.
func optionKind(o string) sdk.PermissionOptionKind {
	l := strings.ToLower(strings.TrimSpace(o))
	switch {
	case l == "no" || l == "n" || strings.HasPrefix(l, "no,") || strings.HasPrefix(l, "no "):
		return sdk.PermissionOptionKindRejectOnce
	case strings.HasPrefix(l, "yes, for this project"), strings.HasPrefix(l, "yes, everywhere"):
		return sdk.PermissionOptionKindAllowAlways
	}
	return sdk.PermissionOptionKindAllowOnce
}

// askText shows the question as text; the next prompt answers it.
func (s *session) askText(a *proto.Ask) {
	var b strings.Builder
	b.WriteString("\n\n**dun asks:** " + a.Question + "\n")
	for _, o := range a.Options {
		b.WriteString("- " + o + "\n")
	}
	if a.Multi {
		b.WriteString("(one or more)\n")
	}
	b.WriteString("Your next message is the answer.\n")
	s.mu.Lock()
	s.ask = a
	s.mu.Unlock()
	s.text(b.String())
}

// onAnswered: the question was answered, here or by another client: a
// permission request or form still open for it is withdrawn.
func (s *session) onAnswered(a *proto.Answered) {
	s.mu.Lock()
	var stop context.CancelFunc
	if s.perm != "" && (a.AskID == "" || a.AskID == s.perm) {
		stop = s.permStop
	}
	if s.ask != nil && (a.AskID == "" || a.AskID == s.ask.AskID) {
		s.ask = nil
	}
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}
