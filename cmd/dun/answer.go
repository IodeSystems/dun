package main

import "strings"

// dun -p "task" (USER 2026-10-27): a one-shot for a shell or a script. The
// model is told the caller wants a simple answer after a delimiter; its work
// (tool calls, the text before the delimiter) goes to stderr, and stdout is
// the answer alone. dun -p with no task is the JSON engine, as before.

const answerDelim = "# RESPONSE BELOW"

const answerSystem = "\n\nThis run is `dun -p` from a shell: the user expects a simple response. Do the work " +
	"the task needs, then write a line that is exactly `" + answerDelim + "` and after it only the response: " +
	"plain text, no preamble, no sign-off. Only what follows that line is shown to the user."

// answerOf is what follows the last delimiter line in reply, trimmed; the
// whole reply when the model wrote none.
func answerOf(reply string) string {
	if i := strings.LastIndex(reply, answerDelim); i >= 0 {
		return strings.TrimSpace(reply[i+len(answerDelim):])
	}
	return strings.TrimSpace(reply)
}
