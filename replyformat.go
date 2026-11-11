package dun

import "github.com/iodesystems/bubbleteandmd/prompt"

// replyFormatSystem tells the model how its replies are shown (USER
// 2026-10-27: the harness supports code snippets via markdown ```lang).
// dun's clients render a reply as Markdown (cmd/dun/render.go, glamour), and
// a fenced block names its language for the highlighting.
const replyFormatSystem = "\n\nYour replies are shown as Markdown. Put code, commands, file contents and output " +
	"in fenced blocks that name the language (```go, ```sh, ```json, ```diff): they are syntax-highlighted and keep " +
	"their lines. Use inline `code` for names, paths and short commands."

// formatPromptSystem is bubbleteandmd's section that teaches the directive
// forms its renderer draws (USER 2026-11-11: on by default, a switch to turn
// it off; Math and Diagrams stay off). Fixed text, so it keeps the cache. Not
// for a sub-agent (its reply goes to the parent model, not to a person), not
// under --non-interactive (no one reads along; the Harbor adapter), and not
// with DUN_FORMAT_PROMPT=0 (Config.NoFormatPrompt).
func (h *Harness) formatPromptSystem() string {
	if h.cfg.NoFormatPrompt || h.cfg.NonInteractive || h.isChild() {
		return ""
	}
	return "\n\n" + prompt.Format(prompt.Options{})
}
