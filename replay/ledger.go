package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RoundStat is one model round, one line of $DUN_HOME/stats/rounds-*.jsonl.
// The same shape internal/harness's stats ledger writes (harness.RoundStat); the
// replay package cannot import it without a cycle, so it declares the fields
// it reads itself.
type RoundStat struct {
	Time     string `json:"t"`
	Session  string `json:"session,omitempty"`
	Role     string `json:"role,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Prompt   int    `json:"prompt"`
	Cached   int    `json:"cached,omitempty"`
}

// LoadRoundStats reads every rounds-*.jsonl file in dir and returns the ledger
// lines in file and line order. Unparseable lines are skipped, as the reader in
// cmd/dun/usage.go does.
func LoadRoundStats(dir string) ([]RoundStat, error) {
	files, err := filepath.Glob(filepath.Join(dir, "rounds-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []RoundStat
	for _, f := range files {
		lh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(lh)
		sc.Buffer(make([]byte, 4096), 4096) // statsLineMax is 4000
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var r RoundStat
			if json.Unmarshal([]byte(line), &r) != nil {
				continue
			}
			out = append(out, r)
		}
		lh.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RoundsFor returns the ledger's main rounds for session, in ledger order.
// Only main-role lines carry the prompt the driver builds (sub-agent rounds
// are separate requests), matching the filter cmd/dun/usage.go applies.
func RoundsFor(all []RoundStat, session string) []RoundStat {
	var out []RoundStat
	for _, r := range all {
		if r.Session == session && r.Role == "main" && r.Prompt > 0 {
			out = append(out, r)
		}
	}
	return out
}

// ComparePromptSize reports whether the replayed prompt sizes and the ledger's
// prompt sizes match within the tolerance, per round, in order.
//
// It is the exported comparison of the ledger-reproduction test: the test
// wires up the sessions, the fixture exercises this. Equal length is required
// (one ledger line per replayed round); a pair within tolerance is
// |replayed − ledger| / ledger ≤ tol. Either side empty is no match.
func ComparePromptSize(replayed, ledger []int, tol float64) (ok bool, maxRel float64) {
	if len(replayed) == 0 || len(ledger) == 0 || len(replayed) != len(ledger) {
		return false, 0
	}
	maxRel = 0.0
	for i := range replayed {
		if ledger[i] <= 0 {
			return false, 0
		}
		d := replayed[i] - ledger[i]
		if d < 0 {
			d = -d
		}
		rel := float64(d) / float64(ledger[i])
		if rel > maxRel {
			maxRel = rel
		}
		if rel > tol {
			return false, maxRel
		}
	}
	return true, maxRel
}

// SessionSplit decides, from the session id alone, which side of the
// held-out split a session is on, BEFORE any of its numbers are looked at
// (the plan's anti-overfitting rule): the low bits of sha256(id) pick the
// side at the given fraction. Tuning happens on the tuning side; the result
// is reported on the held-out side.
func SessionSplit(id string, heldOutFraction float64) (heldOut bool) {
	sum := sha256.Sum256([]byte(id))
	v := binary.BigEndian.Uint32(sum[:4])
	return float64(v) < heldOutFraction*(1<<32)
}

// ParseExcludedSessionIDs reads a comma-separated list of session ids to keep
// out of tuning (the two long DeepSeek-flash sessions of the plan). The split
// above is decided before looking; this is an explicit list, because the ids
// are not derivable from the repo (the ledger lives on the machine that ran
// the sessions).
func ParseExcludedSessionIDs(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if id := strings.TrimSpace(part); id != "" {
			out[id] = true
		}
	}
	return out
}

// ParseFraction parses a decimal fraction (0 ≤ v ≤ 1) from a string, for an
// env var. The zero value (empty or unparseable) is the default.
func ParseFraction(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v > 1 {
		return def
	}
	return v
}
