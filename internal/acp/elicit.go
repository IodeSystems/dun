package acp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// dun's questions as elicitation/create forms (docs/acp.md, "Questions"),
// for an editor that advertises elicitation.form. Every ask is one form:
//
//   - "answer": the options as a select (an enum string), several to check
//     (an array of the enum) for a multiple-choice question, or a text field
//     when there are no options; the recommended one(s) are its default;
//   - "other": a text field for the person's own words, when the question
//     has options and takes other answers (not NoCustom). Its _meta is the
//     one JetBrains Air's own agent puts on such a field (an Air debug
//     bundle, 2026-11-24), so Air shows it as the custom answer.
//
// accept sends the answer (other wins over the select; for multiple choice
// the checked ones and the words, joined ", " as the TUI joins them);
// decline sends the question's "no" option when it has one; cancel, decline
// with no "no", or an empty accept leaves the question open as text, the
// next prompt its answer (the TUI's esc parks a question the same way).

const (
	fieldAnswer = "answer"
	fieldOther  = "other"
)

// customAnswerMeta marks the free-text field as the custom answer: the keys
// JetBrains Air's own agent sends.
var customAnswerMeta = sdk.Meta{
	"_askUserQuestionCustomAnswer": json.RawMessage(`true`),
	"jetbrains":                    json.RawMessage(`{"air":{"customAnswer":true}}`),
}

func ptr[T any](v T) *T { return &v }

// elicitRequest is the question as a form, on the tool call that asked.
func elicitRequest(session string, a *proto.Ask) sdk.CreateElicitationRequest {
	scope := &sdk.ElicitationSessionScope{SessionId: sdk.SessionId(session)}
	if a.CallID != "" {
		scope.ToolCallId = ptr(sdk.ToolCallId(a.CallID))
	}
	props := map[string]sdk.ElicitationPropertySchema{}
	var required []string
	custom := len(a.Options) > 0 && !a.NoCustom
	switch {
	case len(a.Options) == 0:
		props[fieldAnswer] = sdk.ElicitationPropertySchema{String: &sdk.ElicitationPropertySchemaString{Type: "string", Title: ptr("Answer")}}
		required = []string{fieldAnswer}
	case a.Multi:
		arr := &sdk.ElicitationPropertySchemaArray{Type: "array", Title: ptr("Choose one or more"),
			Items: sdk.MultiSelectItems{String: &sdk.MultiSelectItemsString{Type: "string", Enum: a.Options}}}
		if len(a.RecommendedAll) > 0 {
			arr.Default = a.RecommendedAll
		}
		props[fieldAnswer] = sdk.ElicitationPropertySchema{Array: arr}
	default:
		str := &sdk.ElicitationPropertySchemaString{Type: "string", Title: ptr("Choose one"), Enum: a.Options}
		if a.Recommended != "" && slices.Contains(a.Options, a.Recommended) {
			str.Default = ptr(a.Recommended)
		}
		props[fieldAnswer] = sdk.ElicitationPropertySchema{String: str}
	}
	if custom {
		props[fieldOther] = sdk.ElicitationPropertySchema{String: &sdk.ElicitationPropertySchemaString{
			Type: "string", Title: ptr("Other"), Description: ptr("Your own answer, in place of the choices"), Meta: customAnswerMeta}}
	} else if len(a.Options) > 0 {
		required = []string{fieldAnswer}
	}
	return sdk.CreateElicitationRequest{Form: &sdk.CreateElicitationForm{
		Mode:    "form",
		Message: a.Question,
		Session: scope,
		RequestedSchema: sdk.ElicitationSchema{
			Type:       sdk.ElicitationSchemaTypeObject,
			Title:      ptr("dun asks"),
			Properties: props,
			Required:   required,
		},
	}}
}

// elicitAnswer is the answer an accepted form gives; "" when it gives none
// the question takes.
func elicitAnswer(a *proto.Ask, content map[string]sdk.ElicitationContentValue) string {
	str := func(k string) string {
		if v, ok := content[k]; ok && v.String != nil {
			return strings.TrimSpace(string(*v.String))
		}
		return ""
	}
	other := str(fieldOther)
	if a.NoCustom && len(a.Options) > 0 {
		other = ""
	}
	if a.Multi && len(a.Options) > 0 {
		var picked []string
		if v, ok := content[fieldAnswer]; ok && v.StringArray != nil {
			for _, o := range *v.StringArray {
				if slices.Contains(a.Options, o) {
					picked = append(picked, o)
				}
			}
		}
		if other != "" {
			picked = append(picked, other)
		}
		return strings.Join(picked, ", ")
	}
	if other != "" {
		return other
	}
	v := str(fieldAnswer)
	if len(a.Options) > 0 && !slices.Contains(a.Options, v) {
		return ""
	}
	return v
}

// rejectOption is the question's "no" (optionKind's reject), or "".
func rejectOption(a *proto.Ask) string {
	for _, o := range a.Options {
		if optionKind(o) == sdk.PermissionOptionKindRejectOnce {
			return o
		}
	}
	return ""
}

// elicit asks the question as a form. An editor that fails the request is
// asked as without elicitation: a permission request, or text.
func (s *session) elicit(ctx context.Context, stop context.CancelFunc, a *proto.Ask) {
	resp, err := s.peer.CreateElicitation(ctx, elicitRequest(s.id, a))
	if err != nil && ctx.Err() == nil && !a.Multi && len(a.Options) > 0 {
		s.log.Info("acp: elicitation/create failed; asking with request_permission", "session", s.id, "err", err)
		s.permission(ctx, stop, a)
		return
	}
	defer stop()
	s.mu.Lock()
	if s.perm == a.AskID {
		s.perm, s.permStop = "", nil
	}
	s.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		// Answered elsewhere, or the turn ended: the SDK sent
		// $/cancel_request, and a late reply is not read.
		return
	case err != nil:
		s.log.Info("acp: elicitation/create failed; asking as text", "session", s.id, "err", err)
		s.askText(a)
	case resp.Accept != nil:
		if v := elicitAnswer(a, resp.Accept.Content); v != "" {
			_ = s.send("", &proto.Answer{Value: v, AskID: a.AskID})
			return
		}
		s.askText(a)
	case resp.Decline != nil:
		if no := rejectOption(a); no != "" {
			_ = s.send("", &proto.Answer{Value: no, AskID: a.AskID})
			return
		}
		s.askText(a)
	default: // cancel, or a future action
		s.askText(a)
	}
}
