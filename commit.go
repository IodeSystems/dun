package dun

import (
	"context"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// Commit messages — the model writes them, the human approves them.
//
// `/worktree commit` used to commit with the literal string "/worktree commit"
// as its message. That is a commit nobody can read later: the diff says what
// changed and the message said nothing at all, so the one artefact git keeps
// forever carried less information than the command that produced it.
//
// So the message is WRITTEN, by one throwaway LLM call that sees the change and
// nothing else it does not need. Three things make that call cheap and safe:
//
//   - It is a TIP session, not the conversation. The messages are built here —
//     rules, intent, diff — rather than appending to the session, so writing a
//     commit message costs one bounded round-trip and leaves no trace in the
//     model's context. The conversation contributes only the last few user
//     messages, as intent; the diff is the authority on what happened.
//   - It is BOUNDED. A large change is truncated, with the porcelain status
//     always intact — the shape of a change survives truncation, and a diff big
//     enough to blow the budget is one where the file list is the better summary
//     anyway.
//   - It is CONFIRMED. Unlike a recap (which is undone by reading a file), a
//     commit message is written once and read forever, so the human sees it and
//     can regenerate or cancel before anything is committed.

// CommitConfig is the `commit` section of dun.json: how the message is written.
type CommitConfig struct {
	// Format names a built-in style. "conventional" (the default) or "plain".
	Format string `json:"format,omitempty"`
	// Instruction replaces the built-in rules with free text handed to the
	// model verbatim, for a project whose convention is its own. It wins over
	// Format, which is why both can be set without a precedence puzzle: the
	// specific statement beats the named one.
	Instruction string `json:"instruction,omitempty"`
}

// The default we suggest. Conventional Commits, because it is the convention
// most repos that have one have, and because the type/scope prefix is the part
// a model gets reliably right while a human reads the subject line anyway.
const commitConventionalRules = `Write the message in Conventional Commits form:

  type(scope): subject

  body

- type is one of feat, fix, refactor, perf, test, docs, build, ci, chore.
- scope is the area touched (a package, a command, a subsystem). Omit it, with the parentheses, when the change is not confined to one.
- subject is imperative mood, lower case, no trailing period, under 72 characters.
- body explains WHY the change was made and what it replaces — not a restatement of the diff, which the reader already has. Wrap at 72 columns. Omit the body only for a change whose subject is genuinely the whole story.`

const commitPlainRules = `Write the message as:

  subject

  body

- subject is imperative mood, under 72 characters, no trailing period, and no type/scope prefix.
- body explains WHY the change was made — not a restatement of the diff, which the reader already has. Wrap at 72 columns.`

// rules is the style the model is asked to follow.
func (c *CommitConfig) rules() string {
	if c != nil {
		if s := strings.TrimSpace(c.Instruction); s != "" {
			return s
		}
		if strings.EqualFold(strings.TrimSpace(c.Format), "plain") {
			return commitPlainRules
		}
	}
	return commitConventionalRules
}

// commitDiffMax bounds the diff handed to the model. Roughly 6k tokens: enough
// for any change a person would commit in one go, and a ceiling on what a
// runaway generated-file change can cost.
const commitDiffMax = 24000

// commitIntentMessages is how many recent user messages are quoted as intent.
// The diff says what changed; these say why it was asked for. More than a
// handful and the tip session is just the conversation again, expensively.
const commitIntentMessages = 3

// commitIntentChars clips each quoted message. A long paste is context for the
// work, not for its commit message.
const commitIntentChars = 400

// CommitMessage asks the model for a commit message covering wt's uncommitted
// changes. It does not stage or commit anything.
func (h *Harness) CommitMessage(ctx context.Context, wt *Worktree) (string, error) {
	if wt == nil || !wt.IsRepo() {
		return "", fmt.Errorf("not a git repository")
	}
	status := wt.UncommittedStatus()
	if strings.TrimSpace(status) == "" || wt.IsClean() {
		return "", fmt.Errorf("nothing to commit — the tree is clean")
	}
	if h.Session == nil || h.Session.Runner == nil {
		return "", fmt.Errorf("no model attached to this session")
	}

	h.srvMu.Lock()
	cfg := h.cfg.CommitCfg
	h.srvMu.Unlock()

	var b strings.Builder
	b.WriteString("Write the git commit message for the change below.\n\n")
	b.WriteString(cfg.rules())
	if intent := h.recentIntent(ctx); intent != "" {
		b.WriteString("\n\nWhat the session was asked to do (context for the WHY; the diff below is the " +
			"authority on what actually changed):\n" + intent)
	}
	b.WriteString("\n\ngit status --porcelain -b:\n" + strings.TrimSpace(status))
	b.WriteString("\n\n" + wt.pendingDiff(commitDiffMax))
	b.WriteString("\n\nReply with ONLY the commit message — the subject line, a blank line, then the body. " +
		"No code fences, no preamble, no closing remarks, and no trailer lines.")

	ch, err := h.Session.Runner.ChatStream(ctx, []llm.Message{{Role: "user", Content: b.String()}}, nil, nil)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for c := range ch {
		if c.Error != "" {
			return "", fmt.Errorf("%s", c.Error)
		}
		out.WriteString(c.Content)
		if c.Done {
			break
		}
	}
	msg := cleanCommitMessage(out.String())
	if msg == "" {
		return "", fmt.Errorf("the model returned no message")
	}
	return msg, nil
}

// recentIntent quotes the last few user messages. Empty when there are none —
// `/worktree commit` on a fresh session is a normal thing to do, and the diff
// alone is a workable brief.
func (h *Harness) recentIntent(ctx context.Context) string {
	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		return ""
	}
	var picked []string
	for i := len(entries) - 1; i >= 0 && len(picked) < commitIntentMessages; i-- {
		if entries[i].Kind != "user" {
			continue
		}
		s := strings.TrimSpace(entries[i].Content)
		if s == "" {
			continue
		}
		if len(s) > commitIntentChars {
			s = s[:commitIntentChars] + "…"
		}
		picked = append(picked, "- "+strings.ReplaceAll(s, "\n", " "))
	}
	// Oldest first: the order they were said in is the order they make sense in.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return strings.Join(picked, "\n")
}

// commitPreambles are the openers a model adds however firmly it is told not to.
var commitPreambles = []string{
	"here is the commit message:", "here's the commit message:",
	"here is the message:", "commit message:",
}

// cleanCommitMessage strips what a model wraps a message in even when told not
// to: code fences, a "Here is the commit message:" opener, blank padding.
//
// It runs to a FIXED POINT because the wrappers nest in either order, and a
// single pass only ever undoes the outermost one. Caught by the test: a
// preamble outside a fence survived, because the fence check ran first and by
// then the fence was no longer at the start.
func cleanCommitMessage(s string) string {
	s = strings.TrimSpace(s)
	for range 4 { // bounded: each pass must remove something or it stops anyway
		before := s
		for _, p := range commitPreambles {
			if strings.HasPrefix(strings.ToLower(s), p) {
				s = strings.TrimSpace(s[len(p):])
				break
			}
		}
		if strings.HasPrefix(s, "```") {
			if i := strings.Index(s, "\n"); i >= 0 {
				s = s[i+1:]
			} else {
				s = strings.TrimPrefix(s, "```")
			}
			if i := strings.LastIndex(s, "```"); i >= 0 {
				s = s[:i]
			}
			s = strings.TrimSpace(s)
		}
		if s == before {
			break
		}
	}
	return strings.TrimSpace(s)
}
