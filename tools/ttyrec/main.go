// Command ttyrec records a terminal program and renders the recording.
//
//	ttyrec rec [-script walk.json] [-result r.json] -o run.cast -- prog args…
//	ttyrec render [-gif out.gif] [-marks dir] run.cast
//
// rec runs the program in a pseudo-terminal of a fixed size and writes an
// asciicast v2 file. With -script it drives the program from a walk (see
// script.go): keystrokes, typed text, waits for words on the screen, and a
// marker ("m" event) at the end of each step. A vt emulator watches the
// output the whole time, both to answer the program's terminal queries and
// to see the screen the waits test.
//
// render replays a cast through the same emulator and draws it with a real
// monospace font (font.go): an animated GIF with long pauses cut short, and
// for each marker a PNG and the screen's text.
//
// Nothing here is dun-specific; bin/ux is what points it at dun.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "rec":
		err = cmdRec(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "ttyrec: unknown command", os.Args[1])
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ttyrec:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ttyrec rec [flags] -o out.cast -- command [args…]\n       ttyrec render [-gif out.gif] [-marks dir] in.cast")
	os.Exit(2)
}
