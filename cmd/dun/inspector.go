package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The tool inspector is a full-screen overlay opened from a tool-call block in
// convo focus (enter). It shows the call's INPUT and OUTPUT in two bordered,
// independently-focusable sub-frames, each scrollable, with less-style search:
// `/` forward, `?` backward, `n`/`N` repeat/reverse, `g`/`G` top/bottom. The
// inline collapsed preview in the conversation is a glance; this is the drill-in
// for a big result you actually want to read and grep — the human counterpart
// to agentkit's {OUTPUT} (which surfaces the same complete bytes to the model's
// reply). esc / q closes back to the conversation.

var (
	stInspFrame = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	stInspFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("212"))
	stInspLabel = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stInspHit   = lipgloss.NewStyle().Background(lipgloss.Color("212")).Foreground(lipgloss.Color("16")) // current match
	stInspMatch = lipgloss.NewStyle().Background(lipgloss.Color("58")).Foreground(lipgloss.Color("230")) // other matches
)

const (
	inspInput  = 0
	inspOutput = 1
)

// inspPane is one sub-frame: a viewport over pre-wrapped display rows. rows are
// the source of truth for search + highlight; the viewport just windows them.
type inspPane struct {
	label string
	src   string
	rows  []string // src wrapped to the pane's inner width
	vp    viewport.Model
}

// inspector is the overlay state. The active search (query/dir/matches) targets
// the FOCUSED pane, like less searching the visible file.
type inspector struct {
	toolName  string
	panes     [2]inspPane
	focus     int
	search    textinput.Model
	searching bool
	query     string
	dir       int   // +1 forward (/), -1 backward (?)
	matches   []int // focused-pane row indices containing query
	at        int   // cursor into matches
}

func newInspector(name, input, output string) inspector {
	if strings.TrimSpace(input) == "" {
		input = "(no input)"
	}
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}
	se := textinput.New()
	se.Prompt = "/"
	ins := inspector{
		toolName: name,
		dir:      1,
		search:   se,
		focus:    inspOutput, // the result is what you usually came to read
	}
	ins.panes[inspInput] = inspPane{label: "input", src: input}
	ins.panes[inspOutput] = inspPane{label: "output", src: output}
	return ins
}

// wrapToRows splits s into display rows wrapped (and padded) to width w, so long
// lines never clip past the frame and every row fills the width for clean
// highlight bars. ANSI in src is preserved (lipgloss width is ANSI-aware).
func wrapToRows(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	style := lipgloss.NewStyle().Width(w)
	var rows []string
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		rows = append(rows, strings.Split(style.Render(ln), "\n")...)
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	return rows
}

// setSize lays the two frames out to fill w×h: a header + footer line and two
// bordered frames (each = 1 label row + viewport + 2 border rows). Input is
// capped small (it's usually short args); output takes the rest.
func (ins *inspector) setSize(w, h int) {
	ins.search.Width = w - 4
	innerW := w - 2 // inside the border
	if innerW < 1 {
		innerW = 1
	}
	// Total rows = header(1) + footer(1) + 2×[border(2) + label(1) + vp]. So the
	// two viewports share h - 2 - 2*3 = h-8 rows.
	shared := h - 8
	if shared < 2 {
		shared = 2
	}
	ins.panes[inspInput].rows = wrapToRows(ins.panes[inspInput].src, innerW)
	ins.panes[inspOutput].rows = wrapToRows(ins.panes[inspOutput].src, innerW)

	inVP := len(ins.panes[inspInput].rows)
	if cap := shared * 2 / 5; inVP > cap {
		inVP = cap
	}
	if inVP < 1 {
		inVP = 1
	}
	outVP := shared - inVP
	if outVP < 1 {
		outVP = 1
	}
	ins.panes[inspInput].vp = viewport.New(innerW, inVP)
	ins.panes[inspOutput].vp = viewport.New(innerW, outVP)
	ins.recomputeMatches()
	ins.render()
}

// recomputeMatches finds the focused pane's rows containing the query and clamps
// the cursor. Called on commit, focus switch, and n/N wrap.
func (ins *inspector) recomputeMatches() {
	ins.matches, ins.at = nil, 0
	if ins.query == "" {
		return
	}
	q := strings.ToLower(ins.query)
	for i, r := range ins.panes[ins.focus].rows {
		if strings.Contains(strings.ToLower(stripANSI(r)), q) {
			ins.matches = append(ins.matches, i)
		}
	}
}

// jumpTo scrolls the focused pane so match `at` sits near the middle, wrapping
// at either end (less-style continuous n/N).
func (ins *inspector) jumpTo(at int) {
	if len(ins.matches) == 0 {
		return
	}
	if at < 0 {
		at = len(ins.matches) - 1
	} else if at >= len(ins.matches) {
		at = 0
	}
	ins.at = at
	p := &ins.panes[ins.focus]
	off := ins.matches[at] - p.vp.Height/2
	if off < 0 {
		off = 0
	}
	p.vp.SetYOffset(off)
	ins.render()
}

// render sets each viewport's content, highlighting the focused pane's matches
// (the current one brightest).
func (ins *inspector) render() {
	for i := range ins.panes {
		p := &ins.panes[i]
		rows := make([]string, len(p.rows))
		copy(rows, p.rows)
		if i == ins.focus && len(ins.matches) > 0 {
			cur := ins.matches[ins.at]
			for _, mr := range ins.matches {
				if mr >= 0 && mr < len(rows) {
					st := stInspMatch
					if mr == cur {
						st = stInspHit
					}
					rows[mr] = st.Render(stripANSI(rows[mr]))
				}
			}
		}
		p.vp.SetContent(strings.Join(rows, "\n"))
	}
}

// update handles a key while the inspector is open. It returns whether the
// inspector should stay open (false = close back to the conversation).
func (ins *inspector) update(msg tea.KeyMsg) (open bool, cmd tea.Cmd) {
	if ins.searching {
		switch msg.String() {
		case "esc":
			ins.searching = false
			ins.search.Blur()
		case "enter":
			ins.query = strings.TrimSpace(ins.search.Value())
			ins.searching = false
			ins.search.Blur()
			ins.recomputeMatches()
			if ins.dir > 0 {
				ins.jumpTo(0)
			} else {
				ins.jumpTo(len(ins.matches) - 1)
			}
		default:
			ins.search, cmd = ins.search.Update(msg)
		}
		return true, cmd
	}

	switch msg.String() {
	case "esc", "q":
		return false, nil
	case "tab", "left", "right":
		ins.focus ^= 1
		ins.recomputeMatches()
		ins.render()
	case "/":
		ins.searching, ins.dir = true, 1
		ins.search.Prompt = "/"
		ins.search.Reset()
		ins.search.Focus()
		return true, textinput.Blink
	case "?":
		ins.searching, ins.dir = true, -1
		ins.search.Prompt = "?"
		ins.search.Reset()
		ins.search.Focus()
		return true, textinput.Blink
	case "n":
		ins.jumpTo(ins.at + ins.dir)
	case "N":
		ins.jumpTo(ins.at - ins.dir)
	case "g", "home":
		ins.panes[ins.focus].vp.GotoTop()
	case "G", "end":
		ins.panes[ins.focus].vp.GotoBottom()
	default:
		ins.panes[ins.focus].vp, cmd = ins.panes[ins.focus].vp.Update(msg)
	}
	return true, cmd
}

func (ins inspector) view(w, h int) string {
	header := stInspLabel.Render("🔍 "+ins.toolName) +
		stDim.Render("  tab switch · ↑/↓ scroll · / ? search · n/N next · g/G ends · esc close")

	frames := make([]string, 2)
	for i := range ins.panes {
		p := ins.panes[i]
		fr, active := stInspFrame, "  "
		if i == ins.focus {
			fr, active = stInspFocus, stInspLabel.Render("▎ ")
		}
		unit := "lines"
		if len(p.rows) == 1 {
			unit = "line"
		}
		label := active + stInspLabel.Render(p.label) +
			stDim.Render(fmt.Sprintf("  %d %s", len(p.rows), unit))
		frames[i] = fr.Width(w - 2).Render(label + "\n" + p.vp.View())
	}

	var footer string
	switch {
	case ins.searching:
		footer = ins.search.View()
	case len(ins.matches) > 0:
		footer = stDim.Render(fmt.Sprintf("match %d/%d in %s  ·  n/N next/prev",
			ins.at+1, len(ins.matches), ins.panes[ins.focus].label))
	case ins.query != "":
		footer = stDim.Render(fmt.Sprintf("no match for %q in %s", ins.query, ins.panes[ins.focus].label))
	default:
		footer = stDim.Render("frame: " + ins.panes[ins.focus].label)
	}
	return strings.Join([]string{header, frames[inspInput], frames[inspOutput], footer}, "\n")
}
