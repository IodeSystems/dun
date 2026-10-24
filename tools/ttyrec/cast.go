package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// asciicast v2: a header object on the first line, then one event per line,
// [seconds, code, data] — "o" output, "i" input, "m" a marker, "r" a resize.
// https://docs.asciinema.org/manual/asciicast/v2/

type castHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp,omitempty"`
	Command   string            `json:"command,omitempty"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type castEvent struct {
	T    float64
	Code string
	Data string
}

// castWriter appends events with times relative to its start. Safe for
// concurrent use: output and markers come from different goroutines.
type castWriter struct {
	mu    sync.Mutex
	w     *bufio.Writer
	f     *os.File
	start time.Time
}

func createCast(path string, h castHeader) (*castWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	cw := &castWriter{w: bufio.NewWriter(f), f: f, start: time.Now()}
	h.Version = 2
	h.Timestamp = cw.start.Unix()
	b, _ := json.Marshal(h)
	cw.w.Write(b)
	cw.w.WriteByte('\n')
	return cw, nil
}

func (cw *castWriter) event(code, data string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	t := time.Since(cw.start).Seconds()
	b, _ := json.Marshal([]any{roundT(t), code, data})
	cw.w.Write(b)
	cw.w.WriteByte('\n')
}

func roundT(t float64) float64 { return float64(int64(t*1e6)) / 1e6 }

func (cw *castWriter) Close() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if err := cw.w.Flush(); err != nil {
		return err
	}
	return cw.f.Close()
}

// readCast parses a whole cast. Output data is kept as written; a UTF-8
// sequence split across two events is fine because the emulator is fed bytes.
func readCast(r io.Reader) (castHeader, []castEvent, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	var h castHeader
	if !sc.Scan() {
		return h, nil, fmt.Errorf("empty cast")
	}
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		return h, nil, fmt.Errorf("cast header: %w", err)
	}
	if h.Version != 2 {
		return h, nil, fmt.Errorf("cast version %d, want 2", h.Version)
	}
	var evs []castEvent
	line := 1
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil || len(raw) != 3 {
			return h, nil, fmt.Errorf("cast line %d: not [t, code, data]", line)
		}
		var e castEvent
		if json.Unmarshal(raw[0], &e.T) != nil || json.Unmarshal(raw[1], &e.Code) != nil || json.Unmarshal(raw[2], &e.Data) != nil {
			return h, nil, fmt.Errorf("cast line %d: bad event", line)
		}
		evs = append(evs, e)
	}
	return h, evs, sc.Err()
}
