package dun

// Asides: context the model needs the next time it thinks, but never a reason
// to make it think. The tool set changing under a running session is the case
// this exists for.

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// The load-bearing property: an aside must not make a driver run a turn.
// Pending() is what every driver asks, and a turn whose only content is "the
// tool set changed" is a whole round-trip spent on an acknowledgement.
func TestAside_NeverSchedulesATurn(t *testing.T) {
	h := newNoteHarness(t)
	h.Aside("your tools changed — docs is now available: search")
	if h.Pending() != 0 {
		t.Errorf("an aside must not be pending work: Pending()=%d", h.Pending())
	}
	if h.Queued() != 0 {
		t.Errorf("an aside is not a queued user message: Queued()=%d", h.Queued())
	}
	// A flush at the end of a turn publishes news; the aside must survive it
	// rather than being published as an inbox arrival.
	if n := h.flushQueued(); n != 0 {
		t.Errorf("flushQueued published %d aside(s)", n)
	}
	if h.Pending() != 0 {
		t.Errorf("still not pending after a flush: Pending()=%d", h.Pending())
	}
}

// Cheapest delivery: the model is already reading a tool result, so the change
// rides along inside it and costs nothing.
func TestAside_LiftsIntoToolResult(t *testing.T) {
	h := newNoteHarness(t)
	inner := func(ctx context.Context, tc llm.ToolCall) (string, error) {
		h.Aside("your tools changed — docs is now available: search")
		return "wrote 3 lines", nil
	}
	out, err := withLiftedQueue(inner, h)(context.Background(), llm.ToolCall{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[session] your tools changed") {
		t.Errorf("aside was not lifted into the tool result: %q", out)
	}
	if h.Pending() != 0 {
		t.Errorf("nothing should be left buffered: %d", h.Pending())
	}
}

// Next-cheapest: a turn is about to run anyway (the user typed something), so
// the aside is appended just before it and read in the same request — joined
// with the user message rather than preceding it as its own turn.
func TestAside_JoinsTheNextTurn(t *testing.T) {
	h := newNoteHarness(t)
	h.Aside("your tools changed — code was turned off")
	h.prepareTurn(context.Background())

	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != agent.KindNotification {
		t.Fatalf("aside did not land in the conversation: %+v", entries)
	}
	if !strings.Contains(entries[0].Content, "code was turned off") {
		t.Errorf("wrong content: %q", entries[0].Content)
	}
	// Appended, NOT published: an inbox arrival here would schedule the very
	// turn this whole path exists to avoid.
	if h.store.pending() != 0 {
		t.Errorf("aside was published as an inbox arrival: pending=%d", h.store.pending())
	}
	// And it is consumed — a second turn must not re-read it.
	h.prepareTurn(context.Background())
	entries, _ = h.store.Context(context.Background(), "dun")
	if len(entries) != 1 {
		t.Errorf("aside delivered twice: %d entries", len(entries))
	}
}

// Buffered news still behaves as news when an aside is sitting next to it: the
// two are separated by KIND, not by arrival order.
func TestAside_DoesNotSwallowRealNews(t *testing.T) {
	h := newNoteHarness(t)
	h.Aside("your tools changed — docs is now available: search")
	h.Notify("background job #1 finished")
	if h.Pending() != 1 {
		t.Fatalf("the notification is still pending work: Pending()=%d", h.Pending())
	}
	if n := h.flushQueued(); n != 1 {
		t.Errorf("flushQueued should publish the notification only, got %d", n)
	}
	if h.store.pending() != 1 {
		t.Errorf("notification did not reach the inbox: %d", h.store.pending())
	}
	// The aside is still waiting for a free ride.
	if got := h.liftQueued("tool output"); !strings.Contains(got, "[session]") {
		t.Errorf("aside was lost by the flush: %q", got)
	}
}

// The delta is what the tools block cannot say: names that appeared and names
// that went away, grouped by the server the user turned on or off.
func TestToolSetDelta(t *testing.T) {
	code := []mcpmgr.MCPTool{{Name: "node_query", ServerID: ServerCode}}
	docs := []mcpmgr.MCPTool{{Name: "search", ServerID: ServerDocs}, {Name: "ingest", ServerID: ServerDocs}}

	if got := toolSetDelta(code, code); got != "" {
		t.Errorf("no change should say nothing, got %q", got)
	}
	added := toolSetDelta(code, append(append([]mcpmgr.MCPTool{}, code...), docs...))
	if !strings.Contains(added, "docs is now available") || !strings.Contains(added, "search") {
		t.Errorf("start not described: %q", added)
	}
	if strings.Contains(added, "node_query") {
		t.Errorf("unchanged server should not be mentioned: %q", added)
	}
	removed := toolSetDelta(append(append([]mcpmgr.MCPTool{}, code...), docs...), code)
	if !strings.Contains(removed, "turned off") || !strings.Contains(removed, "search") {
		t.Errorf("stop not described: %q", removed)
	}
}

// End to end: turning a server on mid-session leaves an aside waiting, and
// still nothing for a driver to run a turn about.
func TestStartServer_QueuesAnAsideNotATurn(t *testing.T) {
	if !haveBinary("mcpshell") {
		t.Skip("mcpshell not on PATH")
	}
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir,
		Servers:   []Server{{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", dir}, Timeout: 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.StartServer(context.Background(), ServerShell); err != nil {
		t.Fatal(err)
	}
	if h.Pending() != 0 {
		t.Errorf("turning a server on must not schedule a turn: Pending()=%d", h.Pending())
	}
	out := h.liftQueued("some tool output")
	if !strings.Contains(out, "[session] your tools changed") || !strings.Contains(out, "eval") {
		t.Errorf("no aside was queued for the new tools: %q", out)
	}
}
