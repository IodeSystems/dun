package tui

import (
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// OSC 8 hyperlinks. A URL in prose is wrapped in the escape pair
// `ESC ]8;id=ID;URL ESC \ … ESC ]8;; ESC \`, so cmd/ctrl-click opens it and
// "copy link" gives it whole even where dun's wrap broke it over rows. Every
// row of one link carries the same id (relinkRows), which is what makes a
// terminal treat the pieces as one link.
//
// The rows dun keeps always carry the escapes — drag-copy reads them to know
// which row breaks sit inside one URL (selText) — and View strips them when
// the terminal is not known to understand them (hyperlinksOn).

// hyperlinksOn is whether the escapes reach the terminal. DUN_HYPERLINKS=on|off
// decides; unset, auto: on in terminals known to implement OSC 8 (iTerm2,
// WezTerm, kitty, Ghostty, VS Code, Windows Terminal, VTE >= 0.50), off in
// the rest — including Terminal.app, which printed nothing for it before
// macOS 26, and tmux/screen, which drop it unless configured. Unknown
// terminals are off: a terminal that does not parse OSC 8 may print junk.
var hyperlinksOn = hyperlinkDetect(os.Getenv)

func hyperlinkDetect(env func(string) string) bool {
	switch strings.ToLower(env("DUN_HYPERLINKS")) {
	case "on", "1", "yes", "true":
		return true
	case "off", "0", "no", "false":
		return false
	}
	if env("TMUX") != "" || strings.HasPrefix(env("TERM"), "screen") {
		return false
	}
	switch env("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "ghostty", "vscode":
		return true
	}
	if env("LC_TERMINAL") == "iTerm2" || env("KITTY_WINDOW_ID") != "" || env("WT_SESSION") != "" {
		return true
	}
	if t := env("TERM"); t == "xterm-kitty" || t == "xterm-ghostty" {
		return true
	}
	if v, err := strconv.Atoi(env("VTE_VERSION")); err == nil && v >= 5000 {
		return true
	}
	return false
}

const osc8Close = "\x1b]8;;\x1b\\"

func osc8Open(id, url string) string { return "\x1b]8;id=" + id + ";" + url + "\x1b\\" }

// osc8Re matches an OSC 8 sequence, ended by ST or BEL: params, then url.
var osc8Re = regexp.MustCompile(`\x1b\]8;([^;\x1b\x07]*);([^\x1b\x07]*)(?:\x1b\\|\x07)`)

// stripLinks takes every OSC 8 sequence out of s.
func stripLinks(s string) string {
	if !strings.Contains(s, "\x1b]8;") {
		return s
	}
	return osc8Re.ReplaceAllString(s, "")
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `\x00-\x1f\x7f]+`)

// trimURL takes the sentence's punctuation off the end of a match.
func trimURL(u string) string {
	for {
		n := len(u)
		u = strings.TrimRight(u, ".,;:!?*_")
		if strings.HasSuffix(u, ")") && strings.Count(u, "(") < strings.Count(u, ")") {
			u = u[:len(u)-1]
		}
		if strings.HasSuffix(u, "]") && strings.Count(u, "[") < strings.Count(u, "]") {
			u = u[:len(u)-1]
		}
		if len(u) == n {
			return u
		}
	}
}

// linkifyStyled wraps each URL of s (text with SGR escapes in it) in OSC 8.
// A string that has links already is left as it is.
func linkifyStyled(s string) string {
	if !strings.Contains(s, "http") || strings.Contains(s, "\x1b]8;") {
		return s
	}
	var plain strings.Builder
	var at []int // plain byte -> byte in s
	var state byte
	for i := 0; i < len(s); {
		seq, _, n, ns := ansi.DecodeSequence(s[i:], state, nil)
		state = ns
		if n == 0 {
			break
		}
		if seq[0] != 0x1b && !(seq[0] >= 0x80 && seq[0] < 0xa0) {
			for k := 0; k < len(seq); k++ {
				at = append(at, i)
			}
			plain.WriteString(seq)
		}
		i += n
	}
	at = append(at, len(s))
	p := plain.String()
	locs := urlRe.FindAllStringIndex(p, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for k, loc := range locs {
		u := trimURL(p[loc[0]:loc[1]])
		if len(u) <= len("https://") {
			continue
		}
		end := loc[0] + len(u)
		h := fnv.New32a()
		_, _ = h.Write([]byte(u))
		b.WriteString(s[prev:at[loc[0]]])
		b.WriteString(osc8Open(fmt.Sprintf("dun%x-%d", h.Sum32(), k), u))
		b.WriteString(s[at[loc[0]]:at[end]])
		b.WriteString(osc8Close)
		prev = at[end]
	}
	b.WriteString(s[prev:])
	return b.String()
}

// relinkRows makes the rows of one block each carry whole links: a link
// open at a row's end is closed there and opened again, with the same id, at
// the start of the next row. Without it a terminal draws the wrapped part of
// a link on a row after the close as plain text.
func relinkRows(s string) string {
	if !strings.Contains(s, "\x1b]8;") {
		return s
	}
	rows := strings.Split(s, "\n")
	open := "" // the open sequence of the link running over from the row above
	for i, row := range rows {
		cur := open
		if cur != "" && !strings.HasPrefix(row, "\x1b]8;") {
			row = cur + row
		}
		for _, m := range osc8Re.FindAllStringSubmatch(row, -1) {
			if m[2] == "" {
				cur = ""
			} else {
				cur = m[0]
			}
		}
		if cur != "" {
			row += osc8Close
		}
		rows[i], open = row, cur
	}
	return strings.Join(rows, "\n")
}
