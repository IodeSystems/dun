package dundapi_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/proto"
)

// A copy of internal/dund's test fake (its tests use package internals).

// fakeSpawner starts in-process engines that speak the protocol: a session
// event (the --resume id, or a new one), ready, and per user message an
// echo, a reply and done. "crash" as a message makes it exit at once.
type fakeSpawner struct {
	mu     sync.Mutex
	spawns [][]string // the args of every spawn
	n      atomic.Int64
	fail   error
}

func (f *fakeSpawner) Spawn(workspace string, args []string) (dund.Engine, error) {
	f.mu.Lock()
	f.spawns = append(f.spawns, slices.Clone(args))
	err := f.fail
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("s%d", f.n.Add(1))
	if i := slices.Index(args, "--resume"); i >= 0 {
		id = args[i+1]
	}
	e := &fakeEngine{done: make(chan struct{})}
	var inW io.WriteCloser
	e.in, inW = io.Pipe()
	e.stdin = inW
	e.stdoutR, e.stdoutW = io.Pipe()
	go e.run(id)
	return e, nil
}

type fakeEngine struct {
	in      *io.PipeReader
	stdin   io.WriteCloser
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	once    sync.Once
	slow    bool
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
	e.emit(enc, &proto.Ready{Tools: []string{"exec"}})
	sc := bufio.NewScanner(e.in)
	for sc.Scan() {
		var c proto.Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch p := c.Payload.(type) {
		case *proto.User:
			if p.Content == "crash" {
				return
			}
			if log, ok := strings.CutPrefix(p.Content, "job "); ok {
				e.emit(enc, &proto.Jobs{Jobs: []proto.JobInfo{{ID: 1, Command: "make", Log: log, State: "exited"}}})
				continue
			}
			switch p.Content {
			case "toolpair": // one user message, one tool call and its result, one reply
				e.emit(enc, &proto.UserEcho{ID: "u1", Content: p.Content})
				e.emit(enc, &proto.Turn{Kind: "user"})
				e.emit(enc, &proto.ToolCall{CallID: "ct", Tool: "exec", Args: map[string]any{"cmd": "go test ./..."}})
				e.emit(enc, &proto.ToolResult{CallID: "ct", Tool: "exec", Result: "ok", ElapsedMS: 42})
				e.emit(enc, &proto.Message{Role: "assistant", Content: "done: " + p.Content, ID: "m1"})
				e.emit(enc, &proto.Done{})
				continue
			case "deliveredmsg": // a turn running, a message queued, then lifted into a tool result
				e.echoID = "uq1"
				e.emit(enc, &proto.UserEcho{ID: e.echoID, Content: p.Content, Queued: true})
				e.emit(enc, &proto.Turn{Kind: "user"})
				e.slow = true
				continue
			case "lift": // the turn reads the queued message: its tool result carries it
				if e.slow {
					e.emit(enc, &proto.ToolCall{CallID: "cl", Tool: "exec", Args: map[string]any{"cmd": "make"}})
					e.emit(enc, &proto.ToolResult{CallID: "cl", Tool: "exec", Result: "ok", ElapsedMS: 1})
					e.emit(enc, &proto.Delivered{IDs: []string{e.echoID}, CallID: "cl"})
					e.slow = false
					e.emit(enc, &proto.Done{})
				}
				continue
			}
			e.emit(enc, &proto.UserEcho{ID: "u1", Content: p.Content})
			e.emit(enc, &proto.Turn{Kind: "user"})
			e.emit(enc, &proto.Message{Role: "assistant", Content: "re: " + p.Content, ID: "m1"})
			e.emit(enc, &proto.Done{})
		case *proto.AgentCmd:
			if p.Action == "view" {
				n, _ := strconv.Atoi(p.ID)
				e.emit(enc, &proto.AgentHistory{Agent: n, Prompt: "child"})
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
