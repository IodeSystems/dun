package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
)

// stepResult is one step as the recorder saw it, written to --result.
type stepResult struct {
	ID    string   `json:"id"`
	Mark  bool     `json:"mark"`
	OK    bool     `json:"ok"`
	Error string   `json:"error,omitempty"`
	Did   []string `json:"did"`
	Start float64  `json:"start"`
	End   float64  `json:"end"`
}

type recResult struct {
	Command []string     `json:"command"`
	Cols    int          `json:"cols"`
	Rows    int          `json:"rows"`
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Steps   []stepResult `json:"steps"`
	// Screen is the screen when the run stopped, for a failure's diagnosis.
	Screen string `json:"screen,omitempty"`
}

func cmdRec(args []string) error {
	fl := flag.NewFlagSet("rec", flag.ExitOnError)
	out := fl.String("o", "out.cast", "the asciicast v2 file to write")
	scriptPath := fl.String("script", "", "a walk/script JSON to drive the program (default: forward stdin until it exits)")
	resultPath := fl.String("result", "", "write per-step results here (JSON)")
	cols := fl.Int("cols", 0, "terminal width (default: the script's, else 100)")
	rows := fl.Int("rows", 0, "terminal height (default: the script's, else 30)")
	title := fl.String("title", "", "title for the cast header")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ttyrec rec [flags] -- command [args…]")
		fl.PrintDefaults()
	}
	_ = fl.Parse(args)
	argv := fl.Args()
	if len(argv) == 0 {
		fl.Usage()
		return errors.New("no command")
	}
	var sc *script
	if *scriptPath != "" {
		s, err := loadScript(*scriptPath)
		if err != nil {
			return err
		}
		sc = s
	}
	w, h := 100, 30
	if sc != nil && sc.Cols > 0 {
		w, h = sc.Cols, sc.Rows
	}
	if *cols > 0 {
		w = *cols
	}
	if *rows > 0 {
		h = *rows
	}

	cast, err := createCast(*out, castHeader{Width: w, Height: h, Command: strings.Join(argv, " "), Title: *title,
		Env: map[string]string{"TERM": os.Getenv("TERM"), "SHELL": os.Getenv("SHELL")}})
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		cast.Close()
		return err
	}
	r := &recorder{cast: cast, pty: ptmx, cmd: cmd, lastOut: time.Now()}
	// The emulator answers the program's terminal queries (cursor position,
	// colours, modes) through the pty, as a real terminal would; a TUI that
	// waits for an answer otherwise stalls.
	r.term = newTerm(w, h, ptmx)
	r.done = make(chan struct{})
	go r.pump()

	res := recResult{Command: argv, Cols: w, Rows: h, OK: true}
	if sc == nil {
		go func() { _, _ = io.Copy(ptmx, os.Stdin) }()
		<-r.done
	} else {
		res.Steps, err = r.run(sc)
		if err != nil {
			res.OK, res.Error = false, err.Error()
		}
		res.Screen = r.term.text()
		r.stop()
	}
	_ = cmd.Wait()
	cast.Close()
	if *resultPath != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if werr := os.WriteFile(*resultPath, append(b, '\n'), 0o644); werr != nil && err == nil {
			err = werr
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttyrec: the screen when it stopped:\n%s\n", res.Screen)
	}
	return err
}

type recorder struct {
	cast *castWriter
	pty  *os.File
	cmd  *exec.Cmd
	term *term

	mu      sync.Mutex
	lastOut time.Time
	done    chan struct{}
}

// pump copies the program's output into the cast and the emulator. A UTF-8
// sequence cut by a read is held back until it is whole: the cast is JSON and
// would otherwise carry a replacement character for each half.
func (r *recorder) pump() {
	defer close(r.done)
	buf := make([]byte, 32*1024)
	var held []byte
	for {
		n, err := r.pty.Read(buf)
		if n > 0 {
			data := append(held, buf[:n]...)
			cut := len(data)
			for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
				if utf8.RuneStart(data[i]) {
					if !utf8.FullRune(data[i:]) {
						cut = i
					}
					break
				}
			}
			chunk := data[:cut]
			held = append([]byte(nil), data[cut:]...)
			if len(chunk) > 0 {
				r.cast.event("o", string(chunk))
				_, _ = r.term.Write(chunk)
				r.mu.Lock()
				r.lastOut = time.Now()
				r.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *recorder) sinceOutput() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastOut)
}

func (r *recorder) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *recorder) send(b []byte) error {
	_, err := r.pty.Write(b)
	return err
}

func (r *recorder) screenHas(s string) bool { return screenHas(r.term.text(), s) }

// screenHas says whether the phrase is on the screen, also when the program
// wrapped it: on a narrow terminal "kept your question" can be "kept your" at
// the end of one row and "question" indented on the next, or a long word cut
// mid-word. So it matches the rows as they are, then the rows trimmed and
// joined with one space (all whitespace runs one space, in phrase and screen
// alike), then the rows trimmed and joined with nothing.
func screenHas(screen, phrase string) bool {
	if strings.Contains(screen, phrase) {
		return true
	}
	rows := strings.Split(screen, "\n")
	for i, r := range rows {
		rows[i] = strings.TrimSpace(r)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	p := norm(phrase)
	return strings.Contains(norm(strings.Join(rows, " ")), p) ||
		strings.Contains(strings.Join(rows, ""), phrase)
}

func (r *recorder) run(sc *script) ([]stepResult, error) {
	typing := time.Duration(sc.TypingMS) * time.Millisecond
	if sc.TypingMS == 0 {
		typing = 40 * time.Millisecond
	}
	var results []stepResult
	for _, st := range sc.Steps {
		res := stepResult{ID: st.ID, Mark: !st.NoMark, Start: roundT(time.Since(r.cast.start).Seconds()), Did: []string{}}
		err := r.step(st, typing, &res)
		res.End = roundT(time.Since(r.cast.start).Seconds())
		// The marker goes in even for a failed step: the picture of where it
		// stuck is the useful one.
		if !st.NoMark || err != nil {
			r.cast.event("m", st.ID)
		}
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			return results, fmt.Errorf("step %s: %w", st.ID, err)
		}
		res.OK = true
		results = append(results, res)
	}
	return results, nil
}

func (r *recorder) step(st step, typing time.Duration, res *stepResult) error {
	for _, a := range st.Actions {
		if r.exited() {
			return errors.New("the program exited")
		}
		timeout := time.Duration(a.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 15 * time.Second
		}
		switch {
		case a.Type != nil:
			text := os.ExpandEnv(*a.Type)
			for _, ch := range text {
				if err := r.send([]byte(string(ch))); err != nil {
					return err
				}
				time.Sleep(typing)
			}
			say := a.Say
			if say == "" {
				say = fmt.Sprintf("typed %q", text)
			}
			res.Did = append(res.Did, say)
		case a.Key != "":
			b, _ := keyBytes(a.Key)
			if err := r.send(b); err != nil {
				return err
			}
			time.Sleep(typing)
			say := a.Say
			if say == "" {
				say = "pressed " + keyWords(a.Key)
			}
			res.Did = append(res.Did, say)
		case a.WaitFor != "":
			if err := r.waitUntil(timeout, func() bool { return r.screenHas(a.WaitFor) }); err != nil {
				return fmt.Errorf("waited %s for %q on screen", timeout, a.WaitFor)
			}
		case a.WaitGone != "":
			if err := r.waitUntil(timeout, func() bool { return !r.screenHas(a.WaitGone) }); err != nil {
				return fmt.Errorf("waited %s for %q to leave the screen", timeout, a.WaitGone)
			}
		case a.SleepMS > 0:
			time.Sleep(time.Duration(a.SleepMS) * time.Millisecond)
		}
	}
	settle := time.Duration(st.SettleMS) * time.Millisecond
	if st.SettleMS == 0 {
		settle = 700 * time.Millisecond
	}
	// Quiet for settle, or give up waiting after 5 s (a spinner never stops).
	_ = r.waitUntil(5*time.Second, func() bool { return r.sinceOutput() >= settle })
	return nil
}

func (r *recorder) waitUntil(d time.Duration, ok func() bool) error {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timeout")
		}
		if r.exited() {
			if ok() {
				return nil
			}
			return errors.New("the program exited")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// stop ends the program as closing a terminal window does: SIGHUP to its
// process group, then SIGKILL if it is still there.
func (r *recorder) stop() {
	if r.cmd.Process == nil {
		return
	}
	pgid := r.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGHUP)
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = r.pty.Close()
}
