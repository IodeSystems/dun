package dun

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestLearn_Replay runs the churn detector over recorded sessions, with no
// model call: DUN_CHURN_REPLAY is a glob of dun.jsonl event streams (a
// Harbor trial's agent/dun.jsonl). It reports the episodes per trial, the
// trial's reward when its verifier wrote one, and the first episode's
// lesson prompt. The reward stands in for the task verifying (learn.go's
// gate): lesson calls without the gate (every trial, at most learnCap) and
// with it (passed trials only). Skipped without the variable.
//
//	DUN_CHURN_REPLAY='examples/harbor/jobs/*/*/*/agent/dun.jsonl' go test -run TestLearn_Replay -v
func TestLearn_Replay(t *testing.T) {
	glob := os.Getenv("DUN_CHURN_REPLAY")
	if glob == "" {
		t.Skip("DUN_CHURN_REPLAY is not set")
	}
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files: %q %v", glob, err)
	}
	sort.Strings(files)
	var sample string
	trials, withEp, total, stalls := 0, 0, 0, 0
	ungated, gated := 0, 0
	byReward := map[string][2]int{} // reward: trials, episodes
	for _, f := range files {
		eps, err := replayChurn(f)
		if err != nil {
			t.Logf("%s: %v", f, err)
			continue
		}
		trials++
		total += len(eps)
		for _, e := range eps {
			if e.Stall > 0 {
				stalls++
			}
		}
		if len(eps) > 0 {
			withEp++
			if sample == "" {
				sample = f + "\n" + learnPrompt(eps[0])
			}
		}
		reward := "none"
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(f)), "verifier", "reward.txt")); err == nil {
			reward = strings.TrimSpace(string(b))
		}
		ungated += min(len(eps), learnCap)
		if reward == "1" {
			gated += min(len(eps), learnCap)
		}
		r := byReward[reward]
		byReward[reward] = [2]int{r[0] + 1, r[1] + len(eps)}
		t.Logf("%3d episodes  reward %-4s %s", len(eps), reward, f)
	}
	t.Logf("%d trials, %d with an episode, %d episodes (%d stall); by reward (trials, episodes): %v", trials, withEp, total, stalls, byReward)
	t.Logf("lesson calls (cap %d a trial): %d without the gate, %d with it (passed trials)", learnCap, ungated, gated)
	t.Logf("sample prompt:\n%s", sample)
}

// replayChurn feeds one event stream's tool results, in order, through the
// plan nudge's counter, as withPlanNudge does: a result that starts ERROR,
// or an exec with an [exit: ...] verdict, failed. A user message starts a
// turn. Stalls are approximated from the plan calls, as the stream has no
// plan state: a request is a round; add, update, done, block and drop are
// moves; open is adds less dones and drops; a stall is open nodes and
// planStallRounds rounds since a move (at most planStallMax a turn), and
// the next move resolves it.
func replayChurn(path string) ([]churnEpisode, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	h := &Harness{}
	var calls []churnAttempt
	var out []churnEpisode
	rounds, moved, open, nstall := 0, 0, 0, 0
	var stall *churnEpisode
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e struct {
			Type   string          `json:"type"`
			Tool   string          `json:"tool"`
			Args   json.RawMessage `json:"args"`
			Result string          `json:"result"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "user":
			h.nudge.reset()
			calls = nil
			rounds, moved, nstall, stall = 0, 0, 0, nil
		case "request":
			rounds++
			if stall == nil && open > 0 && nstall < planStallMax && rounds-moved >= defaultPlanStallRounds {
				nstall++
				stall = &churnEpisode{Stall: rounds - moved, Tried: slices.Clone(h.nudge.recent)}
			}
		case "tool_call":
			calls = append(calls, churnAttempt{Tool: e.Tool, Args: string(e.Args)})
			// Recorded sessions use the old single plan tool (action) or the
			// split tools (plan_add, plan_update, plan_close).
			var a struct {
				Action  string `json:"action"`
				Outcome string `json:"outcome"`
			}
			_ = json.Unmarshal(e.Args, &a)
			switch e.Tool {
			case "plan":
			case "plan_add":
				a.Action = "add"
			case "plan_update":
				a.Action = "update"
			case "plan_close":
				a.Action = a.Outcome
			default:
				continue
			}
			switch a.Action {
			case "add":
				open++
			case "done", "drop":
				open = max(0, open-1)
			case "update", "block":
			default:
				continue
			}
			moved = rounds
			if stall != nil {
				stall.Fix = churnAttempt{Tool: e.Tool, Args: clipMiddle(string(e.Args), churnArgsMax)}
				out = append(out, *stall)
				stall = nil
			}
		case "tool_result":
			args := ""
			if len(calls) > 0 {
				args = calls[0].Args
				calls = calls[1:]
			}
			failed := strings.HasPrefix(e.Result, "ERROR") || strings.Contains(e.Result, "[exit: ")
			if ep := h.planNudgeResult(failed, newChurnAttempt(e.Tool, args, e.Result)); ep != nil {
				out = append(out, *ep)
			}
		}
	}
	return out, sc.Err()
}
