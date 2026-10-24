package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
)

// A walk runs at each of its sizes, one run directory per size. The walk
// names them ("sizes": ["48x24", "100x30"]); an older walk's "cols"/"rows"
// is one size; neither is 100×30. --size on bin/ux replaces the list.

type size struct{ Cols, Rows int }

func (s size) String() string { return fmt.Sprintf("%dx%d", s.Cols, s.Rows) }

// parseSize reads "WxH" (or "W×H").
func parseSize(v string) (size, error) {
	w, h, ok := strings.Cut(strings.ReplaceAll(strings.TrimSpace(v), "×", "x"), "x")
	if !ok {
		return size{}, fmt.Errorf("size %q: want COLSxROWS, e.g. 48x24", v)
	}
	c, err1 := strconv.Atoi(w)
	r, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || c < 20 || r < 10 || c > 500 || r > 200 {
		return size{}, fmt.Errorf("size %q: want COLSxROWS, 20–500 by 10–200", v)
	}
	return size{c, r}, nil
}

// walkSizes is the sizes a walk runs at: the override when given, else the
// walk's list, else its cols/rows, else 100×30.
func walkSizes(w walk, override []string) ([]size, error) {
	list := override
	if len(list) == 0 {
		list = w.Sizes
	}
	if len(list) == 0 {
		if w.Cols > 0 && w.Rows > 0 {
			return []size{{w.Cols, w.Rows}}, nil
		}
		return []size{{100, 30}}, nil
	}
	var out []size
	seen := map[size]bool{}
	for _, v := range list {
		s, err := parseSize(v)
		if err != nil {
			return nil, err
		}
		if seen[s] {
			return nil, fmt.Errorf("size %s twice", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// runSizes prints the walk's sizes, one "COLS ROWS" per line, for bin/ux.
func runSizes(args []string) error {
	fl := flag.NewFlagSet("sizes", flag.ExitOnError)
	walkPath := fl.String("walk", "", "the walk JSON")
	var over multi
	fl.Var(&over, "size", "COLSxROWS, replaces the walk's sizes (repeatable)")
	_ = fl.Parse(args)
	if *walkPath == "" {
		return errors.New("need -walk")
	}
	var w walk
	if err := readJSON(*walkPath, &w); err != nil {
		return err
	}
	ss, err := walkSizes(w, over)
	if err != nil {
		return err
	}
	for _, s := range ss {
		fmt.Println(s.Cols, s.Rows)
	}
	return nil
}
