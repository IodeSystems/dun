package dun

// replyFormatSystem tells the model how its replies are shown (USER
// 2026-10-27: the harness supports code snippets via markdown ```lang).
// dun's clients render a reply as Markdown (cmd/dun/render.go, glamour), and
// a fenced block names its language for the highlighting.
const replyFormatSystem = "\n\nYour replies are shown as Markdown. Put code, commands, file contents and output " +
	"in fenced blocks that name the language (```go, ```sh, ```json, ```diff): they are syntax-highlighted and keep " +
	"their lines. Use inline `code` for names, paths and short commands."
