package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The reply Carl saw cut (2026-11-20): glamour broke the URL at
// "vehicle-dwell-", and tmux (no OSC 8) matched each row's piece alone.
const (
	deployURL   = "https://github.com/Reemote/vehicle-dwell-safety/actions/runs/44806756413"
	deployReply = "Deploy is running: run 44806756413 " + deployURL + ", `import_existing_secrets=true`, `data_class=Internal`. I will report the result."
)

// replyRows renders src as a finished reply at width w: the pane's rows
// with their escapes.
func replyRows(t *testing.T, src string, w int) tuiModel {
	t.Helper()
	var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: 40})
	m := mm.(tuiModel)
	m.cur = src
	m.flushCur()
	m.refresh()
	return m
}

// linkPieces is the OSC 8 (id, url) of each row that opens a link to a url
// with prefix pre, and the rows' text with the escapes taken out.
func linkPieces(lines []string, pre string) (ids, urls, texts []string) {
	for _, l := range lines {
		for _, mt := range osc8Re.FindAllStringSubmatchIndex(l, -1) {
			u := l[mt[4]:mt[5]]
			if !strings.HasPrefix(u, pre) {
				continue
			}
			ids, urls = append(ids, l[mt[2]:mt[3]]), append(urls, u)
			rest := l[mt[1]:]
			end := strings.Index(rest, "\x1b]8;;")
			texts = append(texts, ansi.Strip(rest[:end]))
		}
	}
	return
}

func TestReplyLink_FitsMovesWhole(t *testing.T) {
	m := replyRows(t, deployReply, 100)
	_, urls, texts := linkPieces(m.vp.lines, "https://github.com/Reemote")
	if len(urls) != 1 || urls[0] != deployURL || texts[0] != deployURL {
		t.Fatalf("want one piece, the whole url: urls %q texts %q", urls, texts)
	}
	// Without OSC 8 the terminal finds the URL on one row, comma after it.
	var row string
	for _, l := range m.vp.lines {
		if p := ansi.Strip(stripLinks(l)); strings.Contains(p, "github.com") {
			row = p
		}
	}
	if !strings.Contains(row, deployURL+",") || strings.Contains(row, "Deploy") {
		t.Errorf("url not whole on its own row: %q", row)
	}
	// Code spans still render; their backticks never show.
	if strings.Contains(strings.Join(m.vp.lines, "\n"), "`") {
		t.Error("a backtick shows")
	}
}

func TestReplyLink_CutPiecesCarryWholeURL(t *testing.T) {
	for _, tc := range []struct{ name, src, url string }{
		{"bare", deployReply, deployURL},
		{"markdown", "See [the run page](" + deployURL + ") for logs.", deployURL},
	} {
		m := replyRows(t, tc.src, 60)
		ids, urls, texts := linkPieces(m.vp.lines, "https://github.com/Reemote")
		if len(urls) < 2 {
			t.Fatalf("%s: url on %d rows, want it cut at w=60", tc.name, len(urls))
		}
		for i := range urls {
			if urls[i] != tc.url || ids[i] != ids[0] {
				t.Errorf("%s: piece %d id %q url %q", tc.name, i, ids[i], urls[i])
			}
		}
		if tc.name == "bare" {
			if j := strings.Join(texts, ""); j != tc.url {
				t.Errorf("pieces join to %q", j)
			}
		}
		for _, l := range m.vp.lines {
			if sw := ansi.StringWidth(l); sw > 60 {
				t.Errorf("%s: row %d wide", tc.name, sw)
			}
			if strings.Contains(l, nbHyphen) {
				t.Errorf("%s: a held hyphen shows: %q", tc.name, l)
			}
		}
		text := selText(m.vp.lines, cellPos{line: 0, col: 0}, cellPos{line: len(m.vp.lines) - 1, col: 500}, m.gutterW())
		if !strings.Contains(text, tc.url) {
			t.Errorf("%s: drag copy split the url:\n%s", tc.name, text)
		}
	}
}

func TestReplyLink_ShortURLUnchanged(t *testing.T) {
	m := replyRows(t, "Open https://example.com/a-b now.", 100)
	_, urls, texts := linkPieces(m.vp.lines, "https://example.com")
	if len(urls) != 1 || urls[0] != "https://example.com/a-b" || texts[0] != urls[0] {
		t.Errorf("urls %q texts %q", urls, texts)
	}
	var plain []string
	for _, l := range m.vp.lines {
		if p := strings.TrimSpace(ansi.Strip(l)); p != "" {
			plain = append(plain, p)
		}
	}
	if !strings.Contains(strings.Join(plain, "\n"), "Open https://example.com/a-b now.") {
		t.Errorf("rows: %q", plain)
	}
}

func TestHoldURLHyphens(t *testing.T) {
	h := strings.ReplaceAll
	for in, want := range map[string]string{
		"see https://a.io/x-y, ok":   "see <" + h("https://a.io/x-y", "-", nbHyphen) + ">, ok",
		"[t](https://a.io/x-y)":      "[t](" + h("https://a.io/x-y)", "-", nbHyphen),
		"<https://a.io/x-y>":         "<" + h("https://a.io/x-y", "-", nbHyphen) + ">",
		"`https://a.io/x-y`":         "`" + h("https://a.io/x-y", "-", nbHyphen) + "`",
		"```\nhttps://a.io/x-y\n```": "```\nhttps://a.io/x-y\n```",
		"no hyphen https://a.io/x":   "no hyphen https://a.io/x",
		"has ‑ https://a.io/x-y":     "has ‑ https://a.io/x-y",
	} {
		if got, _ := holdURLHyphens(in); got != want {
			t.Errorf("holdURLHyphens(%q) = %q, want %q", in, got, want)
		}
	}
}
