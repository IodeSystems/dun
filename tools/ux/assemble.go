package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The walk file, as far as the run directory needs it. ttyrec reads the same
// file for the actions.
type walk struct {
	Name     string `json:"name"`
	Scenario string `json:"scenario"`
	Role     string `json:"role"`
	Goal     string `json:"goal"`
	Note     string `json:"note"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	Sizes    []string `json:"sizes"` // "48x24", …; replaces cols/rows (sizes.go)
	Steps    []struct {
		ID      string `json:"id"`
		Promise string `json:"promise"`
		NoMark  bool   `json:"no_mark"`
	} `json:"steps"`
}

// What ttyrec wrote: rec's -result and render's render.json.
type recResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Steps []struct {
		ID    string   `json:"id"`
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Did   []string `json:"did"`
	} `json:"steps"`
}

type renderMeta struct {
	Font      string  `json:"font"`
	CellW     int     `json:"cell_w"`
	CellH     int     `json:"cell_h"`
	GIF       string  `json:"gif"`
	GIFFrames int     `json:"gif_frames"`
	Duration  float64 `json:"duration_s"`
	Marks     []struct {
		N     int    `json:"n"`
		Label string `json:"label"`
		PNG   string `json:"png"`
		TXT   string `json:"txt"`
	} `json:"marks"`
}

type runStep struct {
	N       int      `json:"n"`
	ID      string   `json:"id"`
	Promise string   `json:"promise"`
	Did     []string `json:"did"`
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	PNG     string   `json:"png"`
	TXT     string   `json:"txt"`
}

type runFile struct {
	Walk     string            `json:"walk"`
	Scenario string            `json:"scenario"`
	Role     string            `json:"role"`
	Goal     string            `json:"goal"`
	Note     string            `json:"note,omitempty"`
	Stamp    string            `json:"stamp"`
	Product  map[string]string `json:"product"`
	Harness  []string          `json:"harness"`
	Terminal string            `json:"terminal"`
	Font     string            `json:"font"`
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Steps    []runStep         `json:"steps"`
	GIF      string            `json:"gif,omitempty"`
	Cast     string            `json:"cast,omitempty"`
	Files    []string          `json:"files"`
	Brief    []string          `json:"brief_files"`
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, "; ") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func runAssemble(args []string) error {
	fl := flag.NewFlagSet("assemble", flag.ExitOnError)
	walkPath := fl.String("walk", "", "the walk JSON")
	scenPath := fl.String("scenario", "", "the scenario markdown")
	dir := fl.String("run", "", "the run directory (render's -marks output)")
	resPath := fl.String("result", "", "ttyrec rec's -result JSON")
	stamp := fl.String("stamp", "", "the run's time stamp")
	version := fl.String("version", "", "the dun build walked")
	cast := fl.String("cast", "", "the cast file name in the run directory")
	sz := fl.String("size", "", "COLSxROWS this run was recorded at (default: the walk's only size)")
	var notes multi
	fl.Var(&notes, "note", "a fact about how the harness set the run up (repeatable)")
	_ = fl.Parse(args)
	if *walkPath == "" || *scenPath == "" || *dir == "" || *resPath == "" {
		return errors.New("need -walk, -scenario, -run and -result")
	}
	var w walk
	if err := readJSON(*walkPath, &w); err != nil {
		return err
	}
	// The brief describes this run's terminal: one run directory is one size.
	var at size
	if *sz != "" {
		s, err := parseSize(*sz)
		if err != nil {
			return err
		}
		at = s
	} else {
		ss, err := walkSizes(w, nil)
		if err != nil {
			return err
		}
		if len(ss) != 1 {
			return fmt.Errorf("the walk has %d sizes: say which this run is with -size", len(ss))
		}
		at = ss[0]
	}
	w.Cols, w.Rows = at.Cols, at.Rows
	var rr recResult
	if err := readJSON(*resPath, &rr); err != nil {
		return err
	}
	var rm renderMeta
	if err := readJSON(filepath.Join(*dir, "render.json"), &rm); err != nil {
		return err
	}
	scen, err := os.ReadFile(*scenPath)
	if err != nil {
		return err
	}

	promise := map[string]string{}
	for _, s := range w.Steps {
		promise[s.ID] = s.Promise
	}
	res := map[string]int{}
	for i, s := range rr.Steps {
		res[s.ID] = i
	}
	rf := runFile{Walk: w.Name, Scenario: w.Scenario, Role: w.Role, Goal: w.Goal, Note: w.Note, Stamp: *stamp,
		Product: map[string]string{"name": "dun", "version": *version}, Harness: notes,
		Terminal: fmt.Sprintf("%d×%d cells, rendered %d×%d px per cell", w.Cols, w.Rows, rm.CellW, rm.CellH),
		Font:     rm.Font, OK: rr.OK, Error: rr.Error, GIF: rm.GIF, Cast: *cast}
	for _, m := range rm.Marks {
		st := runStep{N: m.N, ID: m.Label, Promise: promise[m.Label], PNG: m.PNG, TXT: m.TXT}
		if i, ok := res[m.Label]; ok {
			st.Did, st.OK, st.Error = rr.Steps[i].Did, rr.Steps[i].OK, rr.Steps[i].Error
		}
		if st.Did == nil {
			st.Did = []string{}
		}
		// The step's text file gets its header: which step, and what was
		// done to reach it, in the person's terms.
		p := filepath.Join(*dir, m.TXT)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		head := fmt.Sprintf("STEP %02d — %s\n", m.N, m.Label)
		if len(st.Did) > 0 {
			head += "You did this to get here: " + strings.Join(st.Did, "; ") + "\n"
		} else {
			head += "You did nothing on this step; you looked.\n"
		}
		if st.Error != "" {
			head += "THE WALK STOPPED HERE: " + st.Error + "\n"
		}
		if err := os.WriteFile(p, []byte(head+"\n"+string(body)), 0o644); err != nil {
			return err
		}
		rf.Steps = append(rf.Steps, st)
	}

	if err := os.WriteFile(filepath.Join(*dir, "BRIEF.md"), []byte(brief(w, rf, string(scen))), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "ACTIONS.md"), []byte(actions(w, rf)), 0o644); err != nil {
		return err
	}
	rf.Brief = []string{"BRIEF.md"}
	for _, s := range rf.Steps {
		rf.Brief = append(rf.Brief, s.PNG, s.TXT)
	}
	rf.Files = append(listDir(*dir), "run.json")
	sort.Strings(rf.Files)
	b, _ := json.MarshalIndent(rf, "", "  ")
	return os.WriteFile(filepath.Join(*dir, "run.json"), append(b, '\n'), 0o644)
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

func listDir(d string) []string {
	es, _ := os.ReadDir(d)
	var out []string
	for _, e := range es {
		if !e.IsDir() && e.Name() != "run.json" {
			out = append(out, e.Name())
		}
	}
	return out
}

// brief is BRIEF.md: the evaluator's whole world. It names every file it may
// read, so "denied the codebase" is a fact about its input rather than an
// instruction it is trusted to obey (canon LEN-2). It explains nothing about
// the product: the person, the goal, the promise each step was meant to keep,
// and the files. If a screen needs explaining, that is the finding. Who reads
// it (Lenny, George, Ives) and what they score lives in their agent
// definitions, so the brief is the same for all three.
func brief(w walk, rf runFile, scenario string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — what you saw\n\n", w.Name)
	b.WriteString("You have never seen this product's source code and cannot look at it. " +
		"**The only files you may read are the ones listed at the bottom of this page, all of them in this directory.**\n\n")
	b.WriteString("## What you are looking at\n\n")
	fmt.Fprintf(&b, "A terminal window, %d columns by %d rows, dark background. Each step below is one moment of it: "+
		"a picture (`.png`) of the window exactly as it looked, and a text file (`.txt`) with every character that was on it. "+
		"The text file starts with what you did to get there, then lists the keys and controls the screen itself names, "+
		"then the whole screen.\n\n", w.Cols, w.Rows)
	b.WriteString("Two things on these screens belong to the test, not to the product, and are not yours to judge: " +
		"the folder path (a temporary folder standing in for your home folder) and the answers the assistant gives " +
		"(a stand-in model gives a fixed answer). Everything else is the product as it ships.\n\n")
	fmt.Fprintf(&b, "## Your goal, today\n\n%s\n\n", w.Goal)
	if w.Role != "" {
		fmt.Fprintf(&b, "Your role: %s\n\n", w.Role)
	}
	if w.Note != "" {
		fmt.Fprintf(&b, "> %s\n\n", w.Note)
	}
	b.WriteString(strings.TrimSpace(scenario) + "\n\n")
	if !rf.OK {
		fmt.Fprintf(&b, "## The walk did not finish\n\n%s. The last step is where it stopped.\n\n", rf.Error)
	}
	b.WriteString("## The steps, and what each was meant to keep\n\n")
	for _, s := range rf.Steps {
		fmt.Fprintf(&b, "### %02d · %s\n\nThe promise: %s\n\n- picture: `%s`\n- what it said: `%s`\n\n", s.N, s.ID, s.Promise, s.PNG, s.TXT)
	}
	b.WriteString("## Every file you may read\n\n- `BRIEF.md`\n")
	for _, s := range rf.Steps {
		fmt.Fprintf(&b, "- `%s`\n- `%s`\n", s.PNG, s.TXT)
	}
	b.WriteString("\nNothing else. Not a source file, not a document elsewhere on this machine, not the other files in this directory.\n\n")
	b.WriteString("## Where your report goes\n\n" +
		"Write it to this directory as `<your name in lower case>.md` (`lenny.md`, `george.md`, `ives.md`). " +
		"Write no other file.\n")
	return b.String()
}

// actions is ACTIONS.md, the runner's half of the action log (LEN-5): what
// the walk did at each step and what the screen offered. It is not in the
// brief: a list of every control on every screen in one place is a map of
// the product the persona is meant to find its way around without one.
func actions(w walk, rf runFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# What the walk did — %s\n\nThe mechanical half of the action log. The persona's half — what they "+
		"would NOT press, and the words that stopped them — comes back in their report.\n\n", w.Name)
	for _, s := range rf.Steps {
		fmt.Fprintf(&b, "## %02d · %s\n\n", s.N, s.ID)
		if len(s.Did) == 0 {
			b.WriteString("- did: nothing, this step only looked\n")
		} else {
			fmt.Fprintf(&b, "- did: %s\n", strings.Join(s.Did, "; "))
		}
		if s.Error != "" {
			fmt.Fprintf(&b, "- STOPPED: %s\n", s.Error)
		}
		b.WriteString("\n")
	}
	return b.String()
}
