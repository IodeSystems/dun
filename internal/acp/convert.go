package acp

import (
	"net/url"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// promptText is an ACP prompt as the text of one dun user message. Text is
// kept as typed; a resource link names its file (the model reads it with
// its tools); an embedded resource is quoted with its URI. An image gets a
// note here: a steering message carries none (promptParts has the images of
// a prompt). Audio is dropped with a note.
func promptText(blocks []sdk.ContentBlock) string {
	text, _ := promptParts(blocks, "", nil)
	return text
}

// promptParts is promptText and the images of the prompt: with a counter,
// each usable image block becomes its label in the text and an image to
// send (images.go). n counts the session's images, so labels do not repeat
// across prompts. With n nil, images are not taken and a note stands.
func promptParts(blocks []sdk.ContentBlock, cwd string, n *int) (string, []proto.Image) {
	var imgs []proto.Image
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, b.Text.Text)
		case b.ResourceLink != nil:
			parts = append(parts, "[file: "+uriPath(b.ResourceLink.Uri)+"]")
		case b.Resource != nil:
			r := b.Resource.Resource
			switch {
			case r.TextResourceContents != nil:
				t := r.TextResourceContents
				parts = append(parts, "["+uriPath(t.Uri)+"]\n```\n"+strings.TrimRight(t.Text, "\n")+"\n```")
			case r.BlobResourceContents != nil:
				parts = append(parts, "["+uriPath(r.BlobResourceContents.Uri)+": binary, not included]")
			}
		case b.Image != nil:
			if n == nil {
				parts = append(parts, "[an image was attached; a steering message cannot carry images]")
				break
			}
			t, img := imageBlock(b.Image, cwd, n)
			parts = append(parts, t)
			if img != nil {
				imgs = append(imgs, *img)
			}
		case b.Audio != nil:
			parts = append(parts, "[audio was attached; dun's ACP bridge does not pass audio]")
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), imgs
}

// uriPath is a file: URI's path, any other URI as it is.
func uriPath(u string) string {
	if p, err := url.Parse(u); err == nil && p.Scheme == "file" && p.Path != "" {
		return p.Path
	}
	return u
}

// planEntries flattens dun's plan tree into ACP's flat list, in tree order.
// A node under several parents is listed once (its repeats are skipped), a
// dropped one not at all. Blocked reads as pending: ACP has no blocked.
func planEntries(p *proto.Plan) []sdk.PlanEntry {
	out := []sdk.PlanEntry{}
	for _, r := range p.Rows {
		if r.Repeat || r.Status == "dropped" {
			continue
		}
		e := sdk.PlanEntry{Content: r.Title, Priority: sdk.PlanEntryPriorityMedium, Status: sdk.PlanEntryStatusPending}
		switch r.Status {
		case "doing":
			e.Status = sdk.PlanEntryStatusInProgress
		case "done":
			e.Status = sdk.PlanEntryStatusCompleted
		}
		if r.Ready && e.Status == sdk.PlanEntryStatusPending {
			e.Priority = sdk.PlanEntryPriorityHigh // it can start now
		}
		out = append(out, e)
	}
	return out
}

// availableCommands is dun's command table as ACP's: a command that takes
// arguments says what in its hint.
func availableCommands(c *proto.Commands) []sdk.AvailableCommand {
	out := []sdk.AvailableCommand{}
	for _, it := range c.Items {
		ac := sdk.AvailableCommand{Name: it.Name, Description: it.Description}
		hint := it.Usage
		if hint == "" && len(it.Choices) > 0 {
			vals := make([]string, len(it.Choices))
			for i, ch := range it.Choices {
				vals[i] = ch.Value
			}
			hint = strings.Join(vals, "|")
		}
		if hint != "" || it.Text {
			if hint == "" {
				hint = "text"
			}
			ac.Input = &sdk.AvailableCommandInput{Unstructured: &sdk.UnstructuredCommandInput{Hint: hint}}
		}
		out = append(out, ac)
	}
	return out
}

// replayItems is a conversation's history as the updates session/load
// streams before it answers: the messages, the thinking, and each tool call
// with its result. Notifications and command lines are dun's own and are
// left out.
func replayItems(items []proto.HistoryItem, cwd string) []sdk.SessionUpdate {
	var out []sdk.SessionUpdate
	for _, it := range items {
		switch it.Kind {
		case "user":
			out = append(out, sdk.UpdateUserMessageText(it.Content))
		case "assistant":
			if strings.TrimSpace(it.Content) != "" {
				out = append(out, sdk.UpdateAgentMessageText(it.Content))
			}
		case "reasoning":
			out = append(out, sdk.UpdateAgentThoughtText(it.Content))
		case "tool_call":
			out = append(out, startCall(&proto.ToolCall{CallID: it.CallID, Tool: it.Tool, Args: it.Args}, cwd))
		case "tool_result":
			out = append(out, endCall(&proto.ToolResult{CallID: it.CallID, Tool: it.Tool, Result: it.Content, IsError: it.IsError, Edits: it.Edits}))
		}
	}
	return out
}
