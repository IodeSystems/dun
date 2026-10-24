package main

// The two outside things a new dun user brings along, stood in for so a walk
// needs no network and no account:
//
// llm serves GET /v1/models (and /models) and POST /v1/chat/completions,
// streamed (SSE) or not. Every answer is canned: the run judges the
// product's screens, not a model's prose.
//
// mcp speaks JSON-RPC 2.0 over stdin/stdout (one message per line) and offers
// one read-only tool, project_notes, that returns a fixed note.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The models the stub lists. Several, because a real endpoint lists several
// and choosing among them is part of what a walk measures.
var models = []string{"chat-fast", "chat-standard", "chat-large", "code-standard", "code-large", "embed-small", "vision-standard"}

func runLLM(args []string) {
	fs := flag.NewFlagSet("llm", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:0", "listen address (port 0: ask the kernel)")
	portFile := fs.String("port-file", "", "write the bound address here once listening")
	logFile := fs.String("log", "", "append one line per request here")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	var reqLog io.Writer = io.Discard
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		reqLog = f
	}
	if *portFile != "" {
		if err := os.WriteFile(*portFile+".tmp", []byte(ln.Addr().String()), 0o644); err != nil {
			log.Fatal(err)
		}
		if err := os.Rename(*portFile+".tmp", *portFile); err != nil {
			log.Fatal(err)
		}
	}
	log.Fatal(http.Serve(ln, llmHandler(reqLog)))
}

func llmHandler(reqLog io.Writer) http.Handler {
	mux := http.NewServeMux()
	listModels := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(reqLog, "%s %s %s\n", time.Now().Format(time.RFC3339), r.Method, r.URL.Path)
		var data []map[string]any
		for _, m := range models {
			data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "uxstub"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
	mux.HandleFunc("GET /v1/models", listModels)
	mux.HandleFunc("GET /models", listModels)
	chat := func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
			return
		}
		reply := cannedReply(req)
		fmt.Fprintf(reqLog, "%s POST %s model=%s stream=%v msgs=%d tools=%d\n",
			time.Now().Format(time.RFC3339), r.URL.Path, req.Model, req.Stream, len(req.Messages), len(req.Tools))
		usage := map[string]any{"prompt_tokens": 1200, "completion_tokens": len(strings.Fields(reply)), "total_tokens": 1200 + len(strings.Fields(reply))}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "stub-1", "object": "chat.completion", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": reply}}},
				"usage": usage,
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		// Word by word, so a recording shows the answer arriving.
		words := strings.SplitAfter(reply, " ")
		for i, wd := range words {
			delta := map[string]any{"content": wd}
			if i == 0 {
				delta["role"] = "assistant"
			}
			send(map[string]any{"id": "stub-1", "object": "chat.completion.chunk", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta}}})
			time.Sleep(25 * time.Millisecond)
		}
		send(map[string]any{"id": "stub-1", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		send(map[string]any{"id": "stub-1", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("POST /chat/completions", chat)
	return mux
}

type chatRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

// cannedReply answers the last user message. dun also makes side calls
// (titles, suggestions); they get a short, neutral line.
func cannedReply(req chatRequest) string {
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		var m struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		}
		if json.Unmarshal(req.Messages[i], &m) == nil && m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				last = s
			} else if b, err := json.Marshal(m.Content); err == nil {
				last = string(b)
			}
			break
		}
	}
	if len(req.Tools) == 0 {
		return "A short answer."
	}
	if strings.Contains(strings.ToLower(last), "project") || strings.Contains(last, "?") {
		return "This project is a small command-line tool. The README says it prints a greeting, " +
			"and main.go holds the whole program. I can read files, run commands in an isolated " +
			"copy of the project, and use the tools you add. What would you like to change first?"
	}
	return "Done. What would you like to do next?"
}

// ---- mcp ----

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func runMCP(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any) {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue // a notification, or junk
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "notes", "version": "1.0.0"},
			})
		case "tools/list":
			reply(m.ID, map[string]any{"tools": []any{map[string]any{
				"name":        "project_notes",
				"description": "Read the team's notes about this project.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}})
		case "tools/call":
			reply(m.ID, map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "Notes: keep the greeting short. Release on Fridays.",
			}}})
		case "ping":
			reply(m.ID, map[string]any{})
		case "prompts/list":
			reply(m.ID, map[string]any{"prompts": []any{}})
		case "resources/list":
			reply(m.ID, map[string]any{"resources": []any{}})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"error": map[string]any{"code": -32601, "message": "method not found: " + m.Method}})
		}
	}
}
