package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"

	filetools "github.com/iodesystems/agentkit/files"
)

// A whole read, bounded by the context left (docs/reading.md, section 2).
//
// A file_read with no range (no offset/limit/at) and no grep returns all of
// a file, past agentkit's 8KB unranged cap (files/read.go), when it fits in
// the room this session has left: the window, less the last prompt, less the
// reply's reserve (outputReserve). Under wholeSmallTokens it is served as is;
// up to 1/wholeRoomShare of the room with the recap cue; up to all of the
// room with a stronger cue (recap it right after); past the room it is
// refused with the numbers and the ways out that apply (USER 2026-11-09:
// refuse only a read that would blow the context). A ranged read is
// agentkit's, with its 64KB cap. This was file_read({whole: true}) until
// USER 2026-11-18 dropped the parameter; a call that still sends it is read
// by its other arguments (wantsWhole).
//
// It wraps agentkit's read rather than changing it: the read is paged with
// ranged reads on the same toolset the call was routed to, so the root,
// the tmp and /file routes, the binary check and the read-before-write
// record are agentkit's own, and agentkit's unranged cap is never reached.
// The description's unranged sentence is dun's (wholeReadDefs).

// The thresholds are starting points (docs/reading.md, "Open"): measure on
// real sessions. The reply's reserve is outputReserve (outbudget.go).
const (
	// wholeSmallTokens: a whole read up to this is served with no cue. It is
	// largeResultChars (recap.go) at four characters a token.
	wholeSmallTokens = 5000
	// wholeRoomShare: a whole read over 1/wholeRoomShare of the room left
	// gets wholeAsideBig instead of wholeAside.
	wholeRoomShare = 4
)

// wholeAside is the cue on a large whole read; wholeAsideBig on one over
// 1/wholeRoomShare of the room.
const (
	wholeAside    = "This read is %d tokens. Take what you need from it, then recap it on your next turn."
	wholeAsideBig = "This read is %d tokens, over a quarter of the room left. Take what you need from it and recap it right after, this turn or the next."
)

// wholeDesc is what an unranged read returns, in file_read's description.
const wholeDesc = "With no offset/limit you get the whole file if it fits the context left"

// wholeReadDefs puts wholeDesc in place of agentkit's unranged sentence (its
// 8KB head) in the read tool's description in defs, or after the
// description if that sentence moved.
func wholeReadDefs(defs []llm.ToolDef, readName string) []llm.ToolDef {
	for i, d := range defs {
		if d.Function.Name != readName {
			continue
		}
		desc := d.Function.Description
		from := strings.Index(desc, "With no offset/limit")
		to := strings.Index(desc[max(from, 0):], "; a ranged read")
		if from >= 0 && to >= 0 {
			desc = desc[:from] + wholeDesc + desc[from+to:]
		} else {
			desc += " " + wholeDesc + "."
		}
		d.Function.Description = desc
		defs[i] = d
	}
	return defs
}

// wholeArgs are the read arguments a whole read looks at. An old call's
// `whole` is not among them: a range wins over it, and without one the read
// is unranged anyway. agentkit ignores the extra field too (a plain
// json.Unmarshal; files/files.go Dispatch).
type wholeArgs struct {
	Path    string          `json:"path"`
	Grep    string          `json:"grep"`
	Pattern string          `json:"pattern"` // agentkit's other name for grep
	Offset  json.RawMessage `json:"offset"`
	Limit   json.RawMessage `json:"limit"`
	At      json.RawMessage `json:"at"`
}

// wantsWhole reports whether a read call is a whole read: no grep and no
// range. As agentkit reads it, offset or limit 0 is no range, and any at is
// one.
func wantsWhole(args string) bool {
	var a wholeArgs
	return json.Unmarshal([]byte(args), &a) == nil && a.Grep == "" && a.Pattern == "" &&
		a.At == nil && !nonZero(a.Offset) && !nonZero(a.Limit)
}

// nonZero reports whether a number argument (agentkit takes it quoted too)
// is given and not 0.
func nonZero(raw json.RawMessage) bool {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return s != "" && s != "null" && s != "0"
}

// wholeRoom is what a whole read is measured against.
type wholeRoom struct {
	known bool
	free  int // window - prompt - reserve
	inUse int // the prompt less its fixed overhead: what a recap can free
	empty int // the room of a context holding only the fixed overhead: a child's
	agent bool
}

// wholeRoom is this session's wholeRoom now. A child's window is taken to be
// this session's: a child runs on the same endpoint unless its model says
// otherwise, and its own window is not known until it starts.
func (h *Harness) wholeRoom() wholeRoom {
	b := h.Window()
	// A child starts only on a plan node (USER 2026-11-10), so delegating
	// needs both tools.
	r := wholeRoom{known: b.Known(), agent: h.hasTool("agent") && h.hasTool(taskgraph.ToolAdd)}
	if !r.known {
		return r
	}
	prompt := max(b.Prompt, b.Overhead)
	r.free = b.Window - prompt - b.Reserved
	r.inUse = prompt - b.Overhead
	r.empty = b.Window - b.Overhead - b.Reserved
	return r
}

// readWhole answers an unranged file_read on route, the toolset withFiles
// chose for it. id is the call's id, for the recap cue.
func (h *Harness) readWhole(ctx context.Context, route *filetools.Toolset, name, args, id string) string {
	var a wholeArgs
	_ = json.Unmarshal([]byte(args), &a)
	abs, err := filetools.Under(route.Root(), a.Path)
	st, serr := os.Stat(abs)
	if err != nil || serr != nil || !st.Mode().IsRegular() || isBinary(abs) {
		// agentkit's refusal, a directory's listing, or its binary answer
		return route.Dispatch(ctx, name, args)
	}
	n, room := h.bytesToTokens(int(st.Size())), h.wholeRoom()
	if msg := wholeVerdict(a.Path, n, room); msg != "" {
		return msg
	}
	out := readPages(ctx, route, name, a.Path)
	if strings.HasPrefix(out, "ERROR: ") {
		return out
	}
	h.keepWhole(out)
	if t := h.meter.Estimate(out); t > wholeSmallTokens {
		// The large-result recap cue (recap.go), raised now instead of once
		// the model has moved on: the key is the one that cue would claim.
		if h.claimCue(bigCueKey(id)) {
			h.AsideWith(fmt.Sprintf(wholeAsideFor(t, room), t), AsideOpts{Key: "recap"})
		}
	}
	return out
}

// wholeAsideFor is the cue's wording for a read of n tokens against r.
func wholeAsideFor(n int, r wholeRoom) string {
	if r.known && n > r.free/wholeRoomShare {
		return wholeAsideBig
	}
	return wholeAside
}

// wholeVerdict is the refusal of a whole read of path, n tokens, against r;
// "" when it may be read.
func wholeVerdict(path string, n int, r wholeRoom) string {
	if n <= wholeSmallTokens {
		return ""
	}
	if !r.known {
		return fmt.Sprintf("ERROR: %s is %d tokens, and the context window is unknown, so a whole read stops at %d. "+
			"Grep it first, or read a range.", path, n, wholeSmallTokens)
	}
	if n <= r.free {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ERROR: %s is %d tokens; %d are free. Ways out:", path, n, max(0, r.free))
	step := 0
	line := func(s string, args ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+s, append([]any{step}, args...)...)
	}
	// Recap frees what is in use, and the room grows by that; it fits once
	// the room is n.
	if need := n - r.free; need <= r.inUse {
		line("Recap what is settled to free at least %d tokens, then read it again.", need)
	}
	if r.agent {
		if n <= r.empty {
			line(`Add a plan node "read %s and report …", then agent({node: "<id>"}): a child spends its own context and returns the findings.`, path)
		} else {
			line("It is larger than a child's whole window: narrow it.")
		}
	}
	line("Grep it first, or read a range.")
	return b.String()
}

// isBinary reports whether path has a zero byte where agentkit's binary
// check looks (the first 8000 bytes, files/read.go binaryScan), so a binary
// file too large for the room gets agentkit's answer, not a size refusal.
func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(f, head)
	return bytes.IndexByte(head[:n], 0) >= 0
}

var (
	pageAll  = regexp.MustCompile(`^(.*): (\d+) lines?$`)
	pageRange = regexp.MustCompile(`^(.*): lines (\d+)-(\d+) of (\d+)$`)
)

// readPages reads all of path through route with ranged reads, one page
// (agentkit's ReadMax) each, and returns it as one read: the header of a
// read that got every line, then the lines. A line longer than a page
// comes back cut, as a ranged read cuts it.
func readPages(ctx context.Context, route *filetools.Toolset, name, path string) string {
	var body strings.Builder
	display, total := "", 0
	for from := 1; ; {
		args, _ := json.Marshal(map[string]any{"path": path, "offset": from})
		out := route.Dispatch(ctx, name, string(args))
		head, rest, _ := strings.Cut(out, "\n")
		if from == 1 && pageAll.MatchString(head) || strings.HasPrefix(out, "ERROR: ") {
			return out // all of it in one page, an empty file, or a refusal
		}
		m := pageRange.FindStringSubmatch(head)
		if m == nil {
			return out // not a page: agentkit's format moved; this read is what it gives
		}
		start, last := atoi(m[2]), atoi(m[3])
		display, total = m[1], atoi(m[4])
		if i := strings.LastIndex(rest, "\n[cut at "); i >= 0 {
			rest = rest[:i]
		}
		body.WriteString(rest)
		body.WriteString("\n")
		if last >= total || last < start {
			break
		}
		from = last + 1
	}
	return fmt.Sprintf("%s: %d lines\n%s", display, total, strings.TrimRight(body.String(), "\n"))
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
