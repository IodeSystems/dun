package procps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The raglit daemon's indexes come from `raglit status --json --all`
// (raglit v0.10.0, GET /api/status): the indexes the caller's token can
// read, and Partial when it could not read all of them.

// RaglitTimeout bounds one `raglit status` run.
const RaglitTimeout = 2 * time.Second

// RaglitNeeds is the line under the daemon's row when raglit is too old.
const RaglitNeeds = "raglit status: needs raglit ≥ v0.10.0"

// RaglitPartial is the line under the daemon's row when some indexes are
// hidden from dun's token. Never shown when Partial is false.
const RaglitPartial = "partial: some indexes are not readable with dun's token"

// RaglitReading is one `raglit status --json --all`.
type RaglitReading struct {
	Rag     *proto.ProcRag
	Partial bool
}

type raglitStatus struct {
	Partial bool `json:"partial"`
	Embed   struct {
		Backend   string `json:"backend"`
		Model     string `json:"model"`
		InFlight  int    `json:"in_flight"`
		BackedOff bool   `json:"backed_off"`
	} `json:"embed"`
	Indexes []struct {
		Project       string    `json:"project"`
		Index         string    `json:"index"`
		State         string    `json:"state"`
		QueueLength   int       `json:"queue_length"`
		Running       int       `json:"running"`
		Files         int64     `json:"files"`
		Chunks        int64     `json:"chunks"`
		FilesLastMin  int64     `json:"files_last_minute"`
		EmbedCallsMin int64     `json:"embed_calls_last_minute"`
		EmbedCalls    int64     `json:"embed_calls"`
		EmbedSeconds  float64   `json:"embed_seconds"`
		EmbedTokens   int64     `json:"embed_tokens_estimated"`
		EmbedInFlight int       `json:"embed_in_flight"`
		LastError     string    `json:"last_error"`
		LastErrorAt   time.Time `json:"last_error_at"`
		LastActivity  time.Time `json:"last_activity"`
	} `json:"indexes"`
}

// ParseRaglitStatus converts the JSON of `raglit status --json --all`; now
// dates the idle seconds.
func ParseRaglitStatus(b []byte, now time.Time) (*RaglitReading, error) {
	var s raglitStatus
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	rag := &proto.ProcRag{Embed: proto.ProcRagEmbed{
		Model: s.Embed.Model, Where: s.Embed.Backend, InFlight: s.Embed.InFlight, BackedOff: s.Embed.BackedOff,
	}}
	for _, x := range s.Indexes {
		name := x.Project + "/" + x.Index
		if x.Project == "" {
			name = "global/" + x.Index
		}
		ix := proto.ProcRagIndex{
			Name: name, Project: x.Project, Kind: x.Index, State: x.State,
			Queue: x.QueueLength, Running: x.Running, EmbedInFlight: x.EmbedInFlight,
			FilesMin: x.FilesLastMin, EmbedsMin: x.EmbedCallsMin,
			Files: x.Files, Chunks: x.Chunks, Embeds: x.EmbedCalls,
			EmbedMs: int64(x.EmbedSeconds * 1000), Tokens: x.EmbedTokens,
			LastErr: x.LastError, LastErrAt: x.LastErrorAt, LastActive: x.LastActivity, IdleSec: -1,
		}
		if !x.LastActivity.IsZero() {
			ix.IdleSec = max(0, int64(now.Sub(x.LastActivity).Seconds()))
		}
		rag.Indexes = append(rag.Indexes, ix)
	}
	return &RaglitReading{Rag: rag, Partial: s.Partial}, nil
}

// RaglitAddr is the daemon's address from its --addr (the first one), as a
// URL; "" when it has none (raglit then uses its own default).
func RaglitAddr(argv []string) string {
	addr := ""
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, "--addr="); ok {
			addr = v
		} else if a == "--addr" && i+1 < len(argv) {
			addr = argv[i+1]
		}
	}
	addr, _, _ = strings.Cut(addr, ",")
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return addr
}

// ReadRaglit runs the daemon's own binary (else raglit on PATH) as
// `status --json --all` against the daemon, with env (RAGLIT_TOKEN) added.
func ReadRaglit(d *Proc, env []string, now time.Time) (*RaglitReading, error) {
	bin := "raglit"
	if len(d.Argv) > 0 && filepath.IsAbs(d.Argv[0]) {
		bin = d.Argv[0]
	}
	args := []string{"status", "--json", "--all"}
	if a := RaglitAddr(d.Argv); a != "" {
		args = append(args, "--daemon", a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), RaglitTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = 200 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("raglit status: no answer in %s", RaglitTimeout)
	}
	if err != nil {
		m := strings.TrimSpace(stderr.String())
		if strings.Contains(m, "unknown flag") || strings.Contains(m, "flag provided but not defined") ||
			strings.Contains(m, "unknown shorthand") {
			return nil, fmt.Errorf("%s", RaglitNeeds)
		}
		if m != "" {
			return nil, fmt.Errorf("raglit status: %s", oneLine(m))
		}
		return nil, fmt.Errorf("raglit status: %w", err)
	}
	r, err := ParseRaglitStatus(out, now)
	if err != nil {
		return nil, fmt.Errorf("%s (%v)", RaglitNeeds, err)
	}
	return r, nil
}

// AttachRaglit gives the raglit daemon's row its indexes (as AttachRag), or
// the dim line saying why not; Partial adds its line.
func AttachRaglit(rows []Row, pid int, r *RaglitReading, rerr error, sessions []proto.ProcSession, now time.Time) {
	if rerr != nil {
		for i := range rows {
			if rows[i].PID == pid {
				rows[i].RagNote = append(rows[i].RagNote, rerr.Error())
			}
		}
		return
	}
	if r == nil {
		return
	}
	AttachRag(rows, pid, r.Rag, sessions, now)
	if r.Partial {
		for i := range rows {
			if rows[i].PID == pid {
				rows[i].RagNote = append(rows[i].RagNote, RaglitPartial)
			}
		}
	}
}
