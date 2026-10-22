package dundapi_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
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
			e.emit(enc, &proto.UserEcho{Content: p.Content})
			e.emit(enc, &proto.Turn{Kind: "user"})
			e.emit(enc, &proto.Message{Role: "assistant", Content: "re: " + p.Content})
			e.emit(enc, &proto.Done{})
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
