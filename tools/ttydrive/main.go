// Command ttydrive runs a terminal program in a pseudo-terminal of a fixed size,
// feeds it a script of keystrokes on stdin, and dumps the emulated screen — so a
// TUI (bubbletea and friends) can be driven and inspected NON-interactively
// (tests, CI, an agent watching what it's doing).
//
//	printf 'waitfor ready\ndump\nkey down\nkey enter\nwait 300\ndump\n' \
//	  | ttydrive --cols 100 --rows 30 dun -tui --workspace ./x
//
// A vt10x terminal emulator parses the child's output into a screen grid, so
// `dump` prints readable text, not escape-code soup.
//
// Script directives (one per line; # and blank lines ignored):
//
//	send <text>          write text literally (\n, \t, \e escapes honored)
//	type <text>          alias for send
//	key  <name>...       send named keys: enter tab esc space up down left right
//	                     backspace delete home end pgup pgdn ctrl-c ctrl-<x>
//	wait <ms>            sleep
//	waitfor <substring>  poll the screen until it appears (5s timeout)
//	resize <cols> <rows> resize the terminal (delivers SIGWINCH)
//	dump                 print the current screen
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

func main() {
	cols := flag.Int("cols", 100, "terminal width")
	rows := flag.Int("rows", 30, "terminal height")
	settle := flag.Int("settle", 250, "ms to wait for output to settle before an implicit final dump")
	waitTimeout := flag.Int("wait-timeout", 15, "waitfor timeout in seconds (bump for TUIs slow to boot)")
	flag.Parse()
	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ttydrive [--cols N --rows N] <command> [args…]  < script")
		os.Exit(2)
	}

	term := vt10x.New(vt10x.WithSize(*cols, *rows))

	cmd := exec.Command(argv[0], argv[1:]...)
	// A stable TERM + skip dun's self-rebuild so a drive is deterministic.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "DUN_NO_AUTOBUILD=1")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(*rows), Cols: uint16(*cols)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ttydrive: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // pty.Start → session leader
		}
	}()

	// Feed the child's output into the emulator continuously.
	go func() { _, _ = io.Copy(term, ptmx) }()

	// term.String() locks the state itself — don't Lock() around it (not reentrant).
	screen := func() string { return trimScreen(term.String()) }

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		cmdWord, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		switch cmdWord {
		case "send", "type":
			_, _ = ptmx.WriteString(unescape(arg))
		case "key":
			for _, name := range strings.Fields(arg) {
				b, ok := keyBytes(name)
				if !ok {
					fmt.Fprintf(os.Stderr, "ttydrive: unknown key %q\n", name)
					continue
				}
				_, _ = ptmx.WriteString(b)
			}
		case "wait":
			if ms, e := strconv.Atoi(arg); e == nil {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		case "waitfor":
			waitFor(screen, strings.Trim(arg, `"'`), time.Duration(*waitTimeout)*time.Second)
		case "resize":
			if f := strings.Fields(arg); len(f) == 2 {
				c, _ := strconv.Atoi(f[0])
				r, _ := strconv.Atoi(f[1])
				if c > 0 && r > 0 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(r), Cols: uint16(c)})
					term.Resize(c, r)
				}
			}
		case "dump":
			s := screen()
			if s == "" { // caught a mid-redraw blank frame — retry once
				time.Sleep(120 * time.Millisecond)
				s = screen()
			}
			printScreen(s)
		default:
			fmt.Fprintf(os.Stderr, "ttydrive: unknown directive %q\n", cmdWord)
		}
	}

	// Implicit final dump once output settles.
	time.Sleep(time.Duration(*settle) * time.Millisecond)
	printScreen(screen())
}

func waitFor(screen func() string, target string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(screen(), target) {
			time.Sleep(80 * time.Millisecond) // let the frame finish drawing
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "ttydrive: waitfor %q timed out after %s\n", target, timeout)
}

func printScreen(s string) {
	fmt.Println("┌─ screen ─────────────────────────────────────")
	fmt.Println(s)
	fmt.Println("└──────────────────────────────────────────────")
}

// trimScreen drops trailing spaces per line and trailing blank lines.
func trimScreen(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return strings.Join(lines[:end], "\n")
}

// unescape turns \n \t \e \r \\ into their bytes (for `send`).
func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\e`, "\x1b", `\\`, `\`)
	return r.Replace(s)
}

// keyBytes maps a key name to the bytes a terminal sends for it.
func keyBytes(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "enter", "return", "cr":
		return "\r", true
	case "tab":
		return "\t", true
	case "esc", "escape":
		return "\x1b", true
	case "space":
		return " ", true
	case "backspace", "bs":
		return "\x7f", true
	case "delete", "del":
		return "\x1b[3~", true
	case "up":
		return "\x1b[A", true
	case "down":
		return "\x1b[B", true
	case "right":
		return "\x1b[C", true
	case "left":
		return "\x1b[D", true
	case "home":
		return "\x1b[H", true
	case "end":
		return "\x1b[F", true
	case "pgup", "pageup":
		return "\x1b[5~", true
	case "pgdn", "pgdown", "pagedown":
		return "\x1b[6~", true
	}
	// ctrl-<letter>
	if strings.HasPrefix(strings.ToLower(name), "ctrl-") && len(name) == 6 {
		c := strings.ToLower(name)[5]
		if c >= 'a' && c <= 'z' {
			return string([]byte{c - 'a' + 1}), true
		}
	}
	return "", false
}
