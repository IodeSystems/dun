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
	if i := slices.Index(args, "--resume"); i >= 0 {
		id = args[i+1]
	}
	e := &fakeEngine{done: make(chan struct{}), sp: f}
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
	slow := false
	for sc.Scan() {
		var c proto.Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch p := c.Payload.(type) {
		case *proto.User:
			e.sp.record("user " + id + ": " + p.Content)
			if js, ok := strings.CutPrefix(p.Content, "peer-request "); ok { // ask dund, as dun_list/dun_msg do
				var req proto.PeerRequest
				_ = json.Unmarshal([]byte(js), &req)
				e.emit(enc, &req)
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
			case "finish":
				if slow {
					slow = false
					e.emit(enc, &proto.Done{})
				}
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
		case *proto.PeerMsg:
			e.sp.record(fmt.Sprintf("peer %s: from %s (%s) wake=%v: %s", id, p.FromName, p.From, p.Wake, p.Text))
		case *proto.PeerReply:
			b, _ := json.Marshal(p)
			e.sp.record("reply " + id + ": " + string(b))
		case *proto.Interrupt:
			e.sp.record("interrupt " + id + ": " + p.Reason)
			if slow {
				slow = false
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
