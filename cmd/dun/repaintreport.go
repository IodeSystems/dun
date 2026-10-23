package main

import (
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// /repaint report: the capture, redacted, plus the model's account of what
// went wrong. Nothing leaves the machine except the one model call, and that
// one asks first when the model is not local.
//
//  1. A deterministic pass (redactor) over every capture file: $HOME, the
//     user and host names, e-mail addresses and token-shaped strings. Its
//     output is prered/, and the prompt is built from it, so the model never
//     sees what this pass can find.
//  2. The model (the session's own, through the engine: dun.RepaintReport)
//     replaces paths, names and conversation text, keeping every row's width
//     and every escape code, and writes explanation.md.
//  3. The same deterministic scan over what came back. A hit is a leak, and
//     is said so; so is a row whose width changed.
//
// Placeholders keep the width of what they replace (fitPlaceholder), so a
// box border after a path stays in its column and the mangling stays where
// it was.

// The files the model gets and gives back. The .txt screens and diff.txt are
// derived from the .ansi ones afterwards, so the model writes each screen
// once.
var reportIn = []string{"diff.txt", "expected.ansi", "actual.ansi", "state.json", "events.jsonl"}
var reportOut = []string{"expected.ansi", "actual.ansi", "state.json", "events.jsonl"}

// escMark stands for the ESC byte in the prompt: a model copies a visible
// glyph faithfully, and a raw control byte it may drop.
const escMark = "⎋"

// ── deterministic redaction ────────────────────────────────────────

// Token-shaped strings: well-known key prefixes, JWTs and bearer values
// (keyRe), and any other long run that mixes upper case, lower case and
// digits (longRe + mixedCase) — a lower-case hex commit id is not a secret.
var keyRe = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})|(?i:\bbearer\s+[A-Za-z0-9._~+/=-]{12,})`)

var longRe = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

func mixedCase(s string) bool {
	var up, lo, dig bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			lo = true
		case r >= '0' && r <= '9':
			dig = true
		}
	}
	return up && lo && dig
}

// redactor knows this machine's names.
type redactor struct {
	home  string
	names []nameRule // user and host names, longest first
}

type nameRule struct {
	re *regexp.Regexp
	ph string
}

// minNameLen: a shorter user or host name is a common word, and replacing it
// everywhere would wreck the capture.
const minNameLen = 3

// newRedactor knows this machine: $HOME, the user name, the host name.
func newRedactor() redactor {
	home, _ := os.UserHomeDir()
	var users, hosts []string
	if u, err := user.Current(); err == nil {
		users = append(users, u.Username)
	}
	users = append(users, os.Getenv("USER"))
	if host, err := os.Hostname(); err == nil {
		hosts = append(hosts, host)
		if short, _, ok := strings.Cut(host, "."); ok {
			hosts = append(hosts, short)
		}
	}
	return newRedactorFor(home, users, hosts)
}

func newRedactorFor(home string, users, hosts []string) redactor {
	r := redactor{}
	if len(home) > 1 {
		r.home = home
	}
	seen := map[string]bool{}
	add := func(n, ph string) {
		if len(n) < minNameLen || seen[n] {
			return
		}
		seen[n] = true
		r.names = append(r.names, nameRule{regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`), ph})
	}
	for _, h := range hosts {
		add(h, "[host]")
	}
	for _, u := range users {
		add(u, "[user]")
	}
	// Longest first: a host's full name before its first label.
	sort.SliceStable(r.names, func(i, j int) bool { return len(r.names[i].re.String()) > len(r.names[j].re.String()) })
	return r
}

// fitPlaceholder is ph at exactly w cells: padded with "_", or cut short.
func fitPlaceholder(ph string, w int) string {
	if n := utf8.RuneCountInString(ph); n < w {
		return ph + strings.Repeat("_", w-n)
	}
	return string([]rune(ph)[:w])
}

func widthOf(s string) int { return utf8.RuneCountInString(s) }

// redact replaces everything the deterministic pass can find, each with a
// placeholder of its own width.
func (r redactor) redact(s string) string {
	if r.home != "" {
		s = strings.ReplaceAll(s, r.home, fitPlaceholder("~", widthOf(r.home)))
	}
	s = keyRe.ReplaceAllStringFunc(s, func(m string) string { return fitPlaceholder("[secret]", widthOf(m)) })
	s = longRe.ReplaceAllStringFunc(s, func(m string) string {
		if !mixedCase(m) {
			return m
		}
		return fitPlaceholder("[secret]", widthOf(m))
	})
	s = emailRe.ReplaceAllStringFunc(s, func(m string) string { return fitPlaceholder("[email]", widthOf(m)) })
	for _, n := range r.names {
		s = n.re.ReplaceAllStringFunc(s, func(m string) string { return fitPlaceholder(n.ph, widthOf(m)) })
	}
	return s
}

// leak is one thing the scan found.
type leak struct {
	file string
	row  int
	what string
}

func (l leak) String() string { return fmt.Sprintf("%s:%d: %s", l.file, l.row, l.what) }

// scan reports, per row, what redact would still replace.
func (r redactor) scan(file, s string) []leak {
	var out []leak
	for i, line := range strings.Split(s, "\n") {
		var what []string
		if r.home != "" && strings.Contains(line, r.home) {
			what = append(what, "home directory")
		}
		if keyRe.MatchString(line) {
			what = append(what, "token-shaped string")
		} else {
			for _, m := range longRe.FindAllString(line, -1) {
				if mixedCase(m) {
					what = append(what, "token-shaped string")
					break
				}
			}
		}
		if emailRe.MatchString(line) {
			what = append(what, "e-mail address")
		}
		for _, n := range r.names {
			if n.re.MatchString(line) {
				what = append(what, strings.Trim(n.ph, "[]")+" name")
			}
		}
		for _, w := range what {
			out = append(out, leak{file, i + 1, w})
		}
	}
	return out
}

// ── local or not ───────────────────────────────────────────────────

// localEndpoint says whether a model URL is on this machine or this network.
//
// dun has no notion of a "local" model elsewhere, so this is the rule: a
// loopback, private (RFC 1918 / ULA) or link-local address, "localhost", a
// name under .local, .lan, .home.arpa or .internal, or a bare name with no
// dot (a name only a LAN resolver answers). No DNS lookup: this runs in
// Update, and a name that needs a lookup to say it is local is not trusted to
// be.
func localEndpoint(u string) bool {
	pu, err := neturl.Parse(u)
	if err != nil {
		return false
	}
	host := strings.ToLower(pu.Hostname())
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suf := range []string{".localhost", ".local", ".lan", ".home.arpa", ".internal"} {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

// ── the report ─────────────────────────────────────────────────────

// startReport runs the deterministic pass, writes the prompt, and — after a
// y/n when the model is not local — asks the engine to send it.
func (m *tuiModel) startReport(dir string) {
	if err := prepareReport(dir, newRedactor()); err != nil {
		m.append(stErr.Render("repaint report: " + err.Error()))
		return
	}
	model := m.model
	if model == "" {
		model = "the session's model"
	}
	send := func(m *tuiModel) {
		m.reportDir = dir
		if !m.proc.controlCmd("repaint", dir) {
			m.reportDir = ""
			m.append(stErr.Render("no engine right now — /reconnect, then /repaint report"))
			return
		}
		m.append(stDim.Render("asked " + model + " to redact the capture and explain it…"))
	}
	if localEndpoint(m.url) {
		send(m)
		return
	}
	m.askConfirm("send the capture to "+model+" for redaction? y/n", send)
}

// prepareReport writes prered/ and prompt.txt.
func prepareReport(dir string, r redactor) error {
	pre := filepath.Join(dir, "prered")
	if err := os.MkdirAll(pre, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || e.Name() == "README.txt" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pre, e.Name()), []byte(r.redact(string(b))), 0o600); err != nil {
			return err
		}
	}
	var b strings.Builder
	b.WriteString(reportPrompt)
	actual := false
	for _, name := range reportIn {
		body, err := os.ReadFile(filepath.Join(pre, name))
		if err != nil {
			continue // no actual.ansi or diff.txt outside tmux
		}
		if name == "actual.ansi" {
			actual = true
		}
		fmt.Fprintf(&b, "=== FILE %s ===\n%s\n=== END ===\n\n", name, strings.TrimRight(strings.ReplaceAll(string(body), "\x1b", escMark), "\n"))
	}
	if !actual {
		b.WriteString("There is no actual.ansi: the terminal could not be read back (not in tmux). Explain from expected.ansi, state.json and events.jsonl only, and say so.\n\n")
	}
	b.WriteString("Now write the output, in the format above.\n")
	return os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(b.String()), 0o600)
}

const reportPrompt = `You are preparing a bug report for dun, a terminal UI (a coding agent's chat window drawn with Bubble Tea, partly through a terminal scroll region). The user saw the screen mangled and ran /repaint, which saved what dun believes it drew (EXPECTED) and what the terminal really showed (ACTUAL, read back from tmux), plus dun's state and its last UI events. The report must be safe to post publicly, and must still show the mangling.

In the .ansi files the character ` + escMark + ` stands for the ESC byte: copy every ` + escMark + ` sequence exactly as it is. Rows are numbered from 1. A first, mechanical pass already replaced the home directory (~___), the user and host names ([user], [host]), e-mail addresses ([email]) and token-shaped strings ([secret]).

TASK 1 — redact. Return each of these files: ` + "expected.ansi, actual.ansi (if given), state.json, events.jsonl" + `. In them replace, with a placeholder of the SAME width:
- file paths, directory and file names, repository and branch names: [path]
- user names, host names, people's names: [user] / [host]
- e-mail addresses: [email]; secrets, API keys, tokens, passwords, cookies: [secret]
- conversation text — what the user typed, what the assistant answered, tool output and code: [text]
The width rule: pad the placeholder with "_" to the number of characters it replaces, or cut it short ("[t", "[pa") when the original is shorter. Change NOTHING else: keep every row, every row's width, spaces, box-drawing and border characters, dun's own labels (status line, "ready", key hints, the divider), every escape sequence, and the garbage itself. Do not repair the mangled rows. A row of junk that is not personal data stays as it is.

TASK 2 — explanation.md, in plain words, for a developer:
- What looks wrong: which rows, where EXPECTED and ACTUAL diverge (quote the rows from diff.txt, already redacted).
- The likely cause, from events.jsonl and state.json: for example bytes written to the terminal from outside dun (text no part of dun draws), a resize dun did not see (compare the sizes in state.json), the scroll region (scroll_region in state.json), or a layout height that does not add up. Say how sure you are.
- How to reproduce it, if the events suggest a way. If they do not, say so.
Use only redacted text in the explanation. Start it with one line "# <short title>".

OUTPUT FORMAT — exactly this, and nothing before or after:
=== FILE expected.ansi ===
<the redacted file>
=== END ===
=== FILE actual.ansi ===
<the redacted file>
=== END ===
=== FILE state.json ===
<the redacted file>
=== END ===
=== FILE events.jsonl ===
<the redacted file>
=== END ===
=== FILE explanation.md ===
<the explanation>
=== END ===

THE CAPTURE:

`

var fileBlockRe = regexp.MustCompile(`(?ms)^=== FILE (\S+) ===\n(.*?)\n?^=== END ===`)

// parseModelFiles splits the model's answer into its files.
func parseModelFiles(out string) map[string]string {
	files := map[string]string{}
	for _, m := range fileBlockRe.FindAllStringSubmatch(out, -1) {
		body := m[2]
		// A model that fences each file anyway: drop the fence lines.
		if lines := strings.Split(body, "\n"); len(lines) >= 2 && strings.HasPrefix(lines[0], "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			body = strings.Join(lines[1:len(lines)-1], "\n")
		}
		files[m[1]] = body
	}
	return files
}

// reportResult is what finishReport found, for the message and for tests.
type reportResult struct {
	redacted    string   // the redacted/ directory
	explanation string   // explanation.md's path ("" if the model wrote none)
	head        []string // its first lines
	title       string
	fallback    []string // files the model did not return: deterministic pass only
	leaks       []leak
	widths      []string // rows whose width the model changed
}

// finishReport turns model-output.txt into redacted/ and explanation.md and
// checks them.
func finishReport(dir string, r redactor) (reportResult, error) {
	res := reportResult{redacted: filepath.Join(dir, "redacted")}
	raw, err := os.ReadFile(filepath.Join(dir, "model-output.txt"))
	if err != nil {
		return res, err
	}
	got := parseModelFiles(string(raw))
	if err := os.MkdirAll(res.redacted, 0o700); err != nil {
		return res, err
	}
	pre := filepath.Join(dir, "prered")
	write := func(name, body string) error {
		return os.WriteFile(filepath.Join(res.redacted, name), []byte(body), 0o600)
	}
	screens := map[string][]string{}
	for _, name := range reportOut {
		before, err := os.ReadFile(filepath.Join(pre, name))
		if err != nil {
			continue // not captured (actual.ansi outside tmux)
		}
		body, ok := got[name]
		if !ok {
			res.fallback = append(res.fallback, name)
			body = string(before)
		} else {
			body = strings.ReplaceAll(body, escMark, "\x1b")
			if !strings.HasSuffix(body, "\n") {
				body += "\n"
			}
		}
		if strings.HasSuffix(name, ".ansi") {
			res.widths = append(res.widths, widthChanges(name, string(before), body)...)
			txt := ansi.Strip(body)
			screens[name] = strings.Split(strings.TrimSuffix(txt, "\n"), "\n")
			if err := write(strings.TrimSuffix(name, ".ansi")+".txt", txt); err != nil {
				return res, err
			}
		}
		if err := write(name, body); err != nil {
			return res, err
		}
	}
	if act, ok := screens["actual.ansi"]; ok {
		if err := write("diff.txt", diffRows(screens["expected.ansi"], act)); err != nil {
			return res, err
		}
	}
	if exp, ok := got["explanation.md"]; ok && strings.TrimSpace(exp) != "" {
		res.explanation = filepath.Join(dir, "explanation.md")
		if err := os.WriteFile(res.explanation, []byte(strings.TrimSpace(exp)+"\n"), 0o600); err != nil {
			return res, err
		}
		lines := strings.Split(strings.TrimSpace(exp), "\n")
		res.head = lines[:min(6, len(lines))]
		res.title = strings.TrimSpace(strings.TrimLeft(lines[0], "# "))
	}
	// The post-check: everything that will be shared, through the same scan.
	ents, _ := os.ReadDir(res.redacted)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(res.redacted, e.Name()))
		res.leaks = append(res.leaks, r.scan("redacted/"+e.Name(), string(b))...)
	}
	if res.explanation != "" {
		b, _ := os.ReadFile(res.explanation)
		res.leaks = append(res.leaks, r.scan("explanation.md", string(b))...)
	}
	return res, nil
}

// widthChanges lists the rows of a screen whose display width the model
// changed, which moves everything after them on the row.
func widthChanges(name, before, after string) []string {
	b := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
	a := strings.Split(strings.TrimSuffix(after, "\n"), "\n")
	var out []string
	if len(a) != len(b) {
		out = append(out, fmt.Sprintf("%s: %d rows, was %d", name, len(a), len(b)))
	}
	for i := 0; i < min(len(a), len(b)); i++ {
		if wa, wb := ansi.StringWidth(a[i]), ansi.StringWidth(b[i]); wa != wb {
			out = append(out, fmt.Sprintf("%s row %d: width %d, was %d", name, i+1, wa, wb))
		}
	}
	return out
}

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// reportEvent is the engine's answer to a /repaint report.
func (m *tuiModel) reportEvent(action, msg string) {
	dir := m.reportDir
	m.reportDir = ""
	if action == "error" {
		m.append(stErr.Render(msg))
		return
	}
	if dir == "" {
		m.append(stDim.Render(msg)) // another client's report
		return
	}
	res, err := finishReport(dir, newRedactor())
	if err != nil {
		m.append(stErr.Render("repaint report: " + err.Error()))
		return
	}
	m.append(res.message())
}

// message is what the person reads: where things are, the explanation's
// first lines, what to check, and the command to file it.
func (res reportResult) message() string {
	var b strings.Builder
	b.WriteString(stHeader.Render("repaint report"))
	b.WriteString("\n  " + stDim.Render("redacted:    ") + tildePath(res.redacted) + "/")
	if res.explanation == "" {
		b.WriteString("\n  " + stErr.Render("the model wrote no explanation.md"))
	} else {
		b.WriteString("\n  " + stDim.Render("explanation: ") + tildePath(res.explanation))
		for _, l := range res.head {
			b.WriteString("\n    " + l)
		}
	}
	if len(res.fallback) > 0 {
		b.WriteString("\n  " + stErr.Render("not returned by the model, deterministic pass only: "+strings.Join(res.fallback, ", ")))
	}
	for _, w := range res.widths {
		b.WriteString("\n  " + stErr.Render("width changed: "+w))
	}
	if len(res.leaks) > 0 {
		b.WriteString("\n  " + stErr.Render(fmt.Sprintf("⚠ the check still finds %d item(s) — do not share before fixing:", len(res.leaks))))
		for i, l := range res.leaks {
			if i == 10 {
				fmt.Fprintf(&b, "\n    … %d more", len(res.leaks)-10)
				break
			}
			b.WriteString("\n    " + l.String())
		}
	} else {
		b.WriteString("\n  " + stDim.Render("check: no home dir, user, host, e-mail or token-shaped string left (conversation text is the model's job — read it before sharing)"))
	}
	if res.explanation != "" {
		title := res.title
		if title == "" {
			title = "TUI screen mangled"
		}
		// --web: gh opens the draft in the browser, and posting is a button
		// there. Without it, gh issue create posts at once.
		b.WriteString("\n" + stNote.Render("file as a GitHub issue? (drafts with gh, asks before posting) — nothing is sent until you run:"))
		b.WriteString("\n  gh issue create --web --repo iodesystems/dun --title " + shq(title) + " --body-file " + shq(res.explanation))
		b.WriteString("\n  " + stDim.Render("attach the files in redacted/ in the browser; gh cannot"))
	}
	return b.String()
}
