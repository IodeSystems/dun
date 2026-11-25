// redo reports the redo-after-shrink metric over the session logs: for each
// shrink event (a main round whose prompt dropped, recorded in the rounds
// ledger), the share of tool calls in the next 20 model rounds that repeat an
// earlier call with the same normalized arguments, overall and per shrink
// kind. It is the F12 measurement (docs/compaction.md, recommendation 7): a
// fold that drops a file read from the visible history shows up as the model
// re-reading it within a few rounds.
//
// It reads two things, both read-only:
//
//   - the rounds ledger, $DUN_HOME/stats/rounds-*.jsonl, for the shrink
//     events (kind "shrink" lines) and the model-round index they landed on;
//   - the session logs, $DUN_HOME/sessions/*/*.jsonl, for the tool calls and
//     the model-round each belongs to.
//
// Usage:
//
//	redo [-home DIR]
//
// -home defaults to $DUN_HOME (or ~/.dun). It prints the metric overall and a
// per-cause table.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/dun/internal/harness"
)

// ledgerRound is one round-stat line: its session, the model-round it is
// (the count of main rounds for that session up to and including it), and the
// shrink cause that landed on it, if any. This number is the same as the
// count of assistant entries in the session log up to that round, so it
// joins with the round a session-log tool call belongs to.
type ledgerRound struct {
	Session string
	Round   int
	Cause   string
}

type shrinkLine struct {
	Session string `json:"session"`
	Cause   string `json:"cause"`
}

type roundLine struct {
	Session string `json:"session"`
	Role    string `json:"role"`
}

type sessionEntry struct {
	Kind     string `json:"Kind"`
	Content  string `json:"Content"`
	ToolName string `json:"ToolName"`
	CreatedAt int64  `json:"CreatedAt"`
}

func main() {
	home := flag.String("home", "", "dun home (default $DUN_HOME or ~/.dun)")
	flag.Parse()

	homeDir := *home
	if homeDir == "" {
		if v := os.Getenv("DUN_HOME"); v != "" {
			homeDir = v
		} else {
			homeDir = filepath.Join(mustHome(), ".dun")
		}
	}

	rounds, err := readLedger(filepath.Join(homeDir, "stats"))
	if err != nil {
		fatalf("ledger: %v", err)
	}
	calls, err := readCalls(filepath.Join(homeDir, "sessions"))
	if err != nil {
		fatalf("sessions: %v", err)
	}

	// Join ledger rounds and session calls by session, per session.
	sessions := map[string]struct {
		shrinks []harness.ShrinkAt
		calls   []harness.ToolCallAt
	}{}
	for _, r := range rounds {
		s := sessions[r.Session]
		if r.Cause != "" {
			s.shrinks = append(s.shrinks, harness.ShrinkAt{Round: r.Round, Cause: r.Cause})
		}
		sessions[r.Session] = s
	}
	for session, cs := range calls {
		s := sessions[session]
		s.calls = cs
		sessions[session] = s
	}

	// Aggregate over all sessions.
	var shrinks []harness.ShrinkAt
	var allCalls []harness.ToolCallAt
	bySession := map[string]harness.RedoShrinkResult{}
	for session, s := range sessions {
		if len(s.shrinks) == 0 {
			continue
		}
		shrinks = append(shrinks, s.shrinks...)
		allCalls = append(allCalls, s.calls...)
		bySession[session] = harness.RedoAfterShrink(s.shrinks, s.calls)
	}
	if len(shrinks) == 0 {
		fmt.Println("no shrink events found in the ledger; nothing to measure.")
		return
	}

	// Per-cause aggregate: sum the per-session per-cause figures.
	perCause := map[string]harness.RedoShrinkCause{}
	for _, r := range bySession {
		for cause, c := range r.PerCause {
			agg := perCause[cause]
			agg.Shrinks += c.Shrinks
			agg.Calls += c.Calls
			agg.Repeats += c.Repeats
			if agg.Calls > 0 {
				agg.Share = float64(agg.Repeats) / float64(agg.Calls)
			}
			perCause[cause] = agg
		}
	}

	totalCalls, totalRepeats := 0, 0
	for _, c := range perCause {
		totalCalls += c.Calls
		totalRepeats += c.Repeats
	}
	totalShare := 0.0
	if totalCalls > 0 {
		totalShare = float64(totalRepeats) / float64(totalCalls)
	}

	fmt.Printf("redo-after-shrink over %d sessions, %d shrink events, window %d rounds\n",
		len(bySession), len(shrinks), harness.RedoShrinkWindow)
	fmt.Printf("calls in shrink windows: %d, repeats of an earlier call: %d (%.1f%%)\n\n",
		totalCalls, totalRepeats, 100*totalShare)

	causes := make([]string, 0, len(perCause))
	for c := range perCause {
		causes = append(causes, c)
	}
	sort.Slice(causes, func(i, j int) bool {
		if perCause[causes[i]].Calls != perCause[causes[j]].Calls {
			return perCause[causes[i]].Calls > perCause[causes[j]].Calls
		}
		return causes[i] < causes[j]
	})
	fmt.Println("  cause     shrinks  calls  repeats  share")
	for _, c := range causes {
		v := perCause[c]
		fmt.Printf("  %-9s %7d %6d %8d %6.1f%%\n", c, v.Shrinks, v.Calls, v.Repeats, 100*v.Share)
	}
}

// readLedger reads the rounds ledger and returns, per session, the model-round
// index of each round and the shrink cause that landed on it. A "shrink" line
// is written just before the round-stat line it lands on, so a pending shrink
// attaches to the next round-stat line for the same session.
func readLedger(dir string) ([]ledgerRound, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "rounds-*.jsonl"))
	var out []ledgerRound
	pending := map[string]string{} // session -> cause of the next main round
	round := map[string]int{}      // session -> main rounds seen
	for _, f := range files {
		lines, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(lines), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var sl shrinkLine
			if json.Unmarshal([]byte(line), &sl) == nil && hasKindShrink(line) {
				pending[sl.Session] = sl.Cause
				continue
			}
			var rl roundLine
			if json.Unmarshal([]byte(line), &rl) != nil || rl.Session == "" {
				continue
			}
			// Only main rounds count as model rounds: a shrink lands on a main
			// round, and the session log's assistant-entry count is that number.
			if rl.Role != "" && rl.Role != "main" {
				continue
			}
			round[rl.Session]++
			out = append(out, ledgerRound{Session: rl.Session, Round: round[rl.Session], Cause: pending[rl.Session]})
			delete(pending, rl.Session)
		}
	}
	return out, nil
}

// hasKindShrink reports whether a ledger line is a shrink event, not a round
// stat: it carries "kind":"shrink".
func hasKindShrink(line string) bool {
	return strings.Contains(line, `"kind":"shrink"`)
}

// readCalls reads every session log and returns, per session, the tool calls
// with the model-round each belongs to. A tool call belongs to the round of
// the assistant entry that preceded it: round = the number of assistant
// entries seen before the call.
func readCalls(dir string) (map[string][]harness.ToolCallAt, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	// Group sidecar files by session stem, as the log and its .recapN.jsonl
	// and .folds share the stem; only the base .jsonl is the session log.
	stems := map[string]string{}
	for _, f := range matches {
		base := filepath.Base(f)
		// A session log is <stem>.jsonl; sidecars are <stem>.recapN.jsonl or
		// under a <stem>.folds/ dir. Keep only the plain log.
		if strings.Contains(base, ".recap") || strings.HasSuffix(filepath.Dir(f), ".folds") {
			continue
		}
		stem := strings.TrimSuffix(base, ".jsonl")
		stems[stem] = f
	}
	out := map[string][]harness.ToolCallAt{}
	for stem, f := range stems {
		calls, err := readSessionCalls(stem, f)
		if err != nil {
			continue
		}
		out[stem] = calls
	}
	return out, nil
}

// readSessionCalls parses one session log into tool calls with their round.
func readSessionCalls(stem, f string) ([]harness.ToolCallAt, error) {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	var entries []sessionEntry
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var e sessionEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt < entries[j].CreatedAt })
	var calls []harness.ToolCallAt
	round := 0
	for _, e := range entries {
		switch e.Kind {
		case "assistant":
			round++
		case "tool_call":
			calls = append(calls, harness.ToolCallAt{
				Round: round,
				Tool:  e.ToolName,
				Key:   harness.NormalizeArgs(e.Content),
			})
		}
	}
	return calls, nil
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		fatalf("home: %v", err)
	}
	return h
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "redo: "+format+"\n", args...)
	os.Exit(1)
}
