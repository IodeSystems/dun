#!/usr/bin/env python3
"""A stand-in model server that records what a coding agent sends.

Run it, point an agent at it, say "hi": every request is saved to OUT as
<n>.json ({"path", "headers", "body"}) and answered with a one-word reply,
so the agent's first request -- its system prompt and tool definitions,
before the person has typed anything that matters -- is on disk to measure
(measure.py). It speaks OpenAI chat completions, OpenAI responses and
Anthropic messages, streamed or not, and lists one model.

    capture.py OUT [PORT]
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

OUT = sys.argv[1]
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8765
os.makedirs(OUT, exist_ok=True)
seq = [0]
REPLY = "ok"


def sse(w, events):
    w.send_response(200)
    w.send_header("content-type", "text/event-stream")
    w.send_header("cache-control", "no-cache")
    w.end_headers()
    for ev in events:
        name, data = ev if isinstance(ev, tuple) else (None, ev)
        if name:
            w.wfile.write(f"event: {name}\n".encode())
        w.wfile.write(f"data: {data if isinstance(data, str) else json.dumps(data)}\n\n".encode())
        w.wfile.flush()


def send_json(w, obj, code=200):
    b = json.dumps(obj).encode()
    w.send_response(code)
    w.send_header("content-type", "application/json")
    w.send_header("content-length", str(len(b)))
    w.end_headers()
    w.wfile.write(b)


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *a):
        sys.stderr.write("capture: " + fmt % a + "\n")

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            send_json(self, {"object": "list", "data": [{"id": "probe", "object": "model", "created": 0, "owned_by": "probe",
                                                         "context_length": 200000}]})
            return
        send_json(self, {}, 404)

    def do_HEAD(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except Exception:
            body = {"_raw": raw.decode("utf-8", "replace")}
        seq[0] += 1
        keep = {k: v for k, v in self.headers.items() if k.lower() not in ("authorization", "x-api-key", "cookie")}
        with open(os.path.join(OUT, f"{seq[0]:03d}.json"), "w") as f:
            json.dump({"path": self.path, "headers": keep, "body": body}, f, indent=1)
        stream = isinstance(body, dict) and body.get("stream")
        p = self.path.split("?")[0]
        now = int(time.time())
        if p.endswith("/messages") and not p.endswith("count_tokens"):
            msg = {"id": "msg_probe", "type": "message", "role": "assistant", "model": body.get("model", "probe"),
                   "content": [{"type": "text", "text": REPLY}], "stop_reason": "end_turn", "stop_sequence": None,
                   "usage": {"input_tokens": 1, "output_tokens": 1}}
            if not stream:
                send_json(self, msg)
                return
            start = dict(msg, content=[], stop_reason=None)
            sse(self, [("message_start", {"type": "message_start", "message": start}),
                       ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                       ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": REPLY}}),
                       ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                       ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 1}}),
                       ("message_stop", {"type": "message_stop"})])
            return
        if p.endswith("count_tokens"):
            send_json(self, {"input_tokens": 1})
            return
        if p.endswith("/responses"):
            item = {"id": "msg_probe", "type": "message", "role": "assistant", "status": "completed",
                    "content": [{"type": "output_text", "text": REPLY, "annotations": []}]}
            resp = {"id": "resp_probe", "object": "response", "created_at": now, "status": "completed", "model": body.get("model", "probe"),
                    "output": [item], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
            if not stream:
                send_json(self, resp)
                return
            sse(self, [("response.created", {"type": "response.created", "response": dict(resp, status="in_progress", output=[])}),
                       ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": dict(item, content=[], status="in_progress")}),
                       ("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_probe", "delta": REPLY}),
                       ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": item}),
                       ("response.completed", {"type": "response.completed", "response": resp})])
            return
        # OpenAI chat completions (and anything else).
        model = body.get("model", "probe") if isinstance(body, dict) else "probe"
        if not stream:
            send_json(self, {"id": "chatcmpl-probe", "object": "chat.completion", "created": now, "model": model,
                             "choices": [{"index": 0, "message": {"role": "assistant", "content": REPLY}, "finish_reason": "stop"}],
                             "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
            return
        chunk = lambda delta, fin=None: {"id": "chatcmpl-probe", "object": "chat.completion.chunk", "created": now, "model": model,
                                         "choices": [{"index": 0, "delta": delta, "finish_reason": fin}]}
        last = chunk({}, "stop")
        last["usage"] = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
        sse(self, [chunk({"role": "assistant", "content": REPLY}), last, "[DONE]"])


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
