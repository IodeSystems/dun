package dun

// serverstate.go — one word for what a server is doing, for /mcp's table
// (mcptable.ServerDetail) and /rag's provider row (indexkind.go). Here, not
// in mcptable, because mcptable imports dun.

import "github.com/iodesystems/dun/internal/textfmt"

// ServerRunWord is what a server is doing: "starting…", "running",
// "stopped · last error: …" or "stopped".
func ServerRunWord(st ServerState) string {
	switch {
	case st.Starting:
		return "starting…"
	case st.Running:
		return "running"
	case st.Err != "":
		return "stopped · last error: " + textfmt.OneLine(st.Err)
	}
	return "stopped"
}

// ServerStateWord is ServerRunWord, but "off" for a server switched off
// that is not running and did not fail: stopped is what an on server is
// between starts.
func ServerStateWord(st ServerState) string {
	if !st.Auto && !st.Starting && !st.Running && st.Err == "" {
		return "off"
	}
	return ServerRunWord(st)
}
