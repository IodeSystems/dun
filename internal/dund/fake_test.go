package dund

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/proto"
)

// fakeSpawner starts in-process engines that speak the protocol: a session
// event (the --resume id, or a new one), ready, and per user message an
// echo, a reply and done. "crash" as a message makes it exit at once.
type fakeSpawner struct {
	mu     sync.Mutex
	spawns [][]string // the args of every spawn
	got    []string   // what the engines received (record)
	n      atomic.Int64
	fail   error
	// slowAuto: a message starting "[automatic] " runs as a slow turn, as
	// a resumed agent's continue would.
	slowAuto atomic.Bool
}

func (f *fakeSpawner) Spawn(workspace string, args []string) (Engine, error) {
	f.mu.Lock()
	f.spawns = append(f.spawns, slices.Clone(args))
	err := f.fail
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("s%d", f.n.Add(1))
	resumed := false
	if i := slices.Index(args, "--resume"); i >= 0 {
		id, resumed = args[i+1], true
	}
	e := &fakeEngine{done: make(chan struct{}), sp: f, resumed: resumed}
	var inW io.WriteCloser
	e.in, inW = io.Pipe()
	e.stdin = inW
	e.stdoutR, e.stdoutW = io.Pipe()
	go e.run(id)
	return e, nil
}

// record notes what an engine received ("user <id>: <text>",
// "interrupt <id>: <reason>").
func (f *fakeSpawner) record(s string) {
	f.mu.Lock()
	f.got = append(f.got, s)
	f.mu.Unlock()
}

func (f *fakeSpawner) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeSpawner) spawnArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.spawns)
}

type fakeEngine struct {
	in      *io.PipeReader
	stdin   io.WriteCloser
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	once    sync.Once
	sp      *fakeSpawner
	resumed bool // started with --resume: it replays a history, as dun does
	echoID  string // set by the toolpair/deliveredmsg cases
}

func (e *fakeEngine) emit(enc *json.Encoder, p any) {
	ev := proto.NewEvent(p)
	ev.TS = time.Now()
	_ = enc.Encode(ev)
}

func (e *fakeEngine) run(id string) {
	defer e.end()
	enc := json.NewEncoder(e.stdoutW)
	e.emit(enc, &proto.Session{ID: id})
	if e.resumed {
		e.emit(enc, &proto.History{Items: []proto.HistoryItem{{Kind: "user", Content: "earlier"}}})
	}
	e.emit(enc, &proto.Ready{Tools: []string{"exec"}})
	sc := bufio.NewScanner(e.in)
	slow, asking := false, false
	for sc.Scan() {
		var c proto.Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch p := c.Payload.(type) {
		case *proto.User:
			e.sp.record("user " + id + ": " + p.Content)
			if f := strings.Fields(p.Content); len(f) == 3 && f[0] == "moved" { // into a worktree: path, branch
				e.emit(enc, &proto.Workspace{Path: f[1], Branch: f[2]})
				continue
			}
			if js, ok := strings.CutPrefix(p.Content, "peer-request "); ok { // ask dund, as dun_list/dun_msg do
				var req proto.PeerRequest
				_ = json.Unmarshal([]byte(js), &req)
				e.emit(enc, &req)
				continue
			}
			if strings.HasPrefix(p.Content, resumePrefix) && e.sp.slowAuto.Load() {
				e.emit(enc, &proto.UserEcho{Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				slow = true
				continue
			}
			switch p.Content {
			case "crash":
				return
			case "slow": // a turn that runs until "finish" or an interrupt
				e.emit(enc, &proto.UserEcho{Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				slow = true
				continue
			case "slowtool": // slow, with a tool call in flight
				e.emit(enc, &proto.UserEcho{ID: "u-" + id, Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				e.emit(enc, &proto.ToolCall{CallID: "c1", Tool: "exec", Args: map[string]any{"cmd": "make deploy"}})
				slow = true
				continue
			case "toolpair": // one user message, one tool call and its result, one reply
				e.emit(enc, &proto.UserEcho{ID: "u-" + id, Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				e.emit(enc, &proto.ToolCall{CallID: "ct", Tool: "exec", Args: map[string]any{"cmd": "go test ./..."}})
				e.emit(enc, &proto.ToolResult{CallID: "ct", Tool: "exec", Result: "ok", ElapsedMS: 42})
				e.emit(enc, &proto.Message{Role: "assistant", Content: "done: " + p.Content, ID: "m-" + id})
				e.emit(enc, &proto.Done{})
				continue
			case "deliveredmsg": // a turn running, a message queued, then lifted into a tool result
				e.echoID = "u-" + id
				e.emit(enc, &proto.UserEcho{ID: e.echoID, Content: p.Content, Queued: true})
				e.emit(enc, &proto.Turn{Kind: "user"})
				slow = true
				continue
			case "lift": // the turn reads the queued message: its tool result carries it
				if slow {
					e.emit(enc, &proto.ToolCall{CallID: "cl", Tool: "exec", Args: map[string]any{"cmd": "make"}})
					e.emit(enc, &proto.ToolResult{CallID: "cl", Tool: "exec", Result: "ok", ElapsedMS: 1})
					e.emit(enc, &proto.Delivered{IDs: []string{e.echoID}, CallID: "cl"})
					slow = false
					e.emit(enc, &proto.Done{})
				}
				continue
			case "agent": // a sub-agent, running until "endagent"
				e.emit(enc, &proto.Agents{Agents: []proto.AgentInfo{{ID: 2, State: "running", Prompt: "fix the build"}}})
				continue
			case "endagent":
				e.emit(enc, &proto.Agents{Agents: []proto.AgentInfo{{ID: 2, State: "idle", Prompt: "fix the build"}}})
				continue
			case "news": // a wake the engine held (proto.UpgradeHold)
				e.emit(enc, &proto.WakeHeld{Count: 1})
				continue
			case "finish":
				if slow {
					slow = false
					e.emit(enc, &proto.Done{})
				}
				continue
			case "ask": // a turn that asks the user (ask_user) and waits for the answer
				e.emit(enc, &proto.UserEcho{Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				e.emit(enc, &proto.Ask{Question: "Which one?", Options: []string{"a", "b"}, NoCustom: true})
				slow, asking = true, true
				continue
			case "job": // a background job, running until "endjob"
				e.emit(enc, &proto.Jobs{Jobs: []proto.JobInfo{{ID: 3, Command: "go test ./...", State: "running"}}})
				continue
			case "endjob":
				e.emit(enc, &proto.Jobs{Jobs: []proto.JobInfo{{ID: 3, Command: "go test ./...", State: "done"}}})
				continue
			}
			e.emit(enc, &proto.UserEcho{Content: p.Content})
			e.emit(enc, &proto.Turn{Kind: "user"})
			e.emit(enc, &proto.Message{Role: "assistant", Content: "re: " + p.Content})
			e.emit(enc, &proto.Done{})
		case *proto.Answer:
			e.sp.record("answer " + id + ": " + p.Value)
			if asking {
				asking, slow = false, false
				e.emit(enc, &proto.Answered{Value: p.Value})
				e.emit(enc, &proto.Done{})
			}
		case *proto.PeerMsg:
			e.sp.record(fmt.Sprintf("peer %s: from %s (%s) wake=%v: %s", id, p.FromName, p.From, p.Wake, p.Text))
			if p.Human {
				e.sp.record("human peer " + id + ": " + p.Text)
			}
		case *proto.AgentCmd:
			e.sp.record("agent " + id + ": " + p.Action + " " + p.ID)
		case *proto.UpgradeHold:
			e.sp.record(fmt.Sprintf("hold %s: %v gen %d", id, p.On, p.Gen))
		case *proto.ServersChanged:
			e.sp.record("servers " + id)
		case *proto.ServiceNotice:
			// dund's notices are quiet (they start no turn); one that would
			// wake the engine is recorded apart, so a test sees it.
			// The real engine shows each to its clients (cmd/dun/peers.go).
			e.emit(enc, &proto.Notification{Kind: "service", Text: p.Text})
			if p.Quiet {
				e.sp.record("notice " + id + ": " + p.Text)
			} else {
				e.sp.record("waking notice " + id + ": " + p.Text)
			}
		case *proto.PeerReply:
			b, _ := json.Marshal(p)
			e.sp.record("reply " + id + ": " + string(b))
		case *proto.Interrupt:
			e.sp.record("interrupt " + id + ": " + p.Reason)
			if slow {
				slow, asking = false, false
				e.emit(enc, &proto.Done{Reason: "interrupted"})
			}
		case *proto.Stop:
			e.emit(enc, &proto.Exit{Reason: "stopped"})
			return
		}
	}
}

func (e *fakeEngine) end() {
	e.once.Do(func() {
		_ = e.stdoutW.Close()
		_ = e.in.Close()
		close(e.done)
	})
}

func (e *fakeEngine) Stdin() io.Writer  { return e.stdin }
func (e *fakeEngine) Stdout() io.Reader { return e.stdoutR }
func (e *fakeEngine) Wait() error       { <-e.done; return nil }
func (e *fakeEngine) Kill()             { e.end() }
